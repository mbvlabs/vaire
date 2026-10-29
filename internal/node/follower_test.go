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
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

func followerCfg() config.Cluster {
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, WGAddress: config.DefaultCentralWG}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s",
			Extensions: []string{"pg_stat_statements"}, Settings: map[string]string{"work_mem": "8MB"},
			Backup: config.ClusterBackup{RetentionFull: 4}},
		{Name: "app_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	return cfg
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var ce *output.CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("not a CLI error: %v", err)
	}
	d, _ := ce.Details.(map[string]any)
	r, _ := d["reason"].(string)
	return r
}

func request(method string, params map[string]any) rpc.Request {
	raw, _ := json.Marshal(params)
	return rpc.Request{Method: method, Params: raw}
}

func TestRefuseFollower(t *testing.T) {
	cfg := followerCfg()
	refused := []rpc.Request{
		request("backup.now", map[string]any{"database": "app_follower"}),
		request("backup.verify", map[string]any{"database": "app_follower"}),
		request("backup.list", map[string]any{"database": "app_follower"}),
		request("restore.start", map[string]any{"database": "app_follower"}),
		request("restore.drill", map[string]any{"database": "app_follower"}),
		request("dump.create", map[string]any{"database": "app_follower"}),
		request("db.fork", map[string]any{"source": "app_follower", "name": "x"}),
		request("maint.run", map[string]any{"db": "app_follower", "kind": "vacuum"}),
		request("db.credentials", map[string]any{"name": "app_follower", "rotate": true}),
	}
	for _, req := range refused {
		err := refuseFollower(req, cfg)
		if err == nil {
			t.Errorf("%s is not refused for a follower", req.Method)
			continue
		}
		if got := reason(t, err); got != "is_follower" {
			t.Errorf("%s: reason %q", req.Method, got)
		}
	}
	allowed := []rpc.Request{
		request("backup.now", map[string]any{"database": "app"}),
		request("db.fork", map[string]any{"source": "app", "name": "x"}),
		request("db.credentials", map[string]any{"name": "app_follower"}),
		request("db.resize", map[string]any{"name": "app_follower", "size": "medium"}),
		request("maint.report", map[string]any{"db": "app_follower"}),
		request("sql", map[string]any{"db": "app_follower"}),
		// D10: a follower may be unfollowed, protected or not. db.follow's
		// refusal of a follower is its handler's (follows_follower), not
		// this list's.
		request("db.unfollow", map[string]any{"name": "app_follower"}),
		request("db.follow", map[string]any{"source": "app", "name": "app_follower"}),
	}
	for _, req := range allowed {
		if err := refuseFollower(req, cfg); err != nil {
			t.Errorf("%s is refused: %v", req.Method, err)
		}
	}
}

func TestRefuseSourceDrop(t *testing.T) {
	cfg := followerCfg()
	err := refuseSourceDrop(cfg, "app")
	if err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("dropping a source with followers: %v", err)
	}
	if err := refuseSourceDrop(cfg, "app_follower"); err != nil {
		t.Fatalf("dropping a follower: %v", err)
	}
}

func TestCheckFollowSourceRefusesCascading(t *testing.T) {
	cfg := followerCfg()
	f, _ := cfg.ClusterByName("app_follower")
	err := checkFollowSource(f, "db follow")
	if err == nil || reason(t, err) != "follows_follower" {
		t.Fatalf("following a follower: %v", err)
	}
}

func TestUnfollowToken(t *testing.T) {
	if got := unfollowToken(config.Cluster{}, json.RawMessage(`{"name":"app_follower"}`)); got != "app_follower/unfollow" {
		t.Fatalf("token %q", got)
	}
	if got := unfollowToken(config.Cluster{}, json.RawMessage(`{}`)); got != "" {
		t.Fatalf("token without a name %q", got)
	}
}

func TestFollowerSpecAndCommand(t *testing.T) {
	cfg := followerCfg()
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "f", "")
	if spec.Follows != "app" || spec.Size != "large" || spec.Stanza != "" || spec.Backup != (config.ClusterBackup{}) || spec.Settings["work_mem"] != "8MB" {
		t.Fatalf("follower spec %+v", spec)
	}
	if small := followerSpec(src, "f", "small"); small.Size != "small" || small.Memory != "" {
		t.Fatalf("--size: %+v", small)
	}
	if got := followerName(dbFollowParams{Source: "app"}); got != "app_follower" {
		t.Fatalf("default name %q", got)
	}
	if got := followCommand(dbFollowParams{Source: "app", Name: "f"})("small"); got != "vaire db follow app --name f --size small" {
		t.Fatalf("suggested command %q", got)
	}
}

func TestPlaceFollowerHasNoStanza(t *testing.T) {
	cfg := followerCfg()
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "f2", "small")
	if err := placeCluster(cfg, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Stanza != "" || spec.Port != 5435 || spec.Node != "db1" {
		t.Fatalf("placed %+v", spec)
	}
	cfg.Clusters = append(cfg.Clusters, spec)
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "clusters[") {
		t.Fatalf("the placed follower does not validate: %v", err)
	}
}

func TestFollowersDiverged(t *testing.T) {
	cfg := followerCfg()
	w := followersDiverged(cfg, "app")
	if len(w) != 1 || w[0].Code != "followers_diverged" || !strings.Contains(w[0].Message, "vaire db follow app --name app_follower") {
		t.Fatalf("%+v", w)
	}
	if followersDiverged(cfg, "app_follower") != nil {
		t.Fatal("a follower has no followers")
	}
}

func TestOwnedSlots(t *testing.T) {
	cfg := followerCfg()
	cfg.Awaiting = []config.ClusterSpec{{Name: "later", Follows: "app"}}
	got := ownedSlots(cfg, "app")
	if len(got) != 2 || got[0] != "vaire_app_follower" || got[1] != "vaire_later" {
		t.Fatalf("owned %v", got)
	}
}

func TestFollowerFamilies(t *testing.T) {
	fams := followerFamilies(map[string]followerReading{
		"app_follower": {Source: "app", Streaming: true, LagBytes: 10, SlotExists: true},
		"old":          {Source: "app", SlotExists: true, SlotWALStatus: "lost"},
	})
	byName := map[string]float64{}
	for _, f := range fams {
		for _, s := range f.Samples {
			byName[f.Name+"/"+s.Labels[0].Value] = s.Value
		}
	}
	if byName["vaire_follower_streaming/app_follower"] != 1 || byName["vaire_follower_lag_bytes/app_follower"] != 10 ||
		byName["vaire_follower_slot_lost/old"] != 1 || byName["vaire_follower_slot_lost/app_follower"] != 0 {
		t.Fatalf("%v", byName)
	}
}

func TestUnfollowRenames(t *testing.T) {
	r := unfollowRenames("app", "app_follower")
	if r["app"] != "app_follower" || r["app_migrate"] != "app_follower_migrate" || r["app_owner"] != "app_follower_owner" {
		t.Fatalf("%v", r)
	}
}

// A completed copy may only be resumed on a slot that still retains WAL: a
// missing, lost, unreserved or restartless slot is not usable, nor is a status
// vaire does not know, while an ordinary inactive slot with an older position
// is.
func TestFollowerSlotState(t *testing.T) {
	for name, test := range map[string]struct {
		rows []map[string]string
		want followerSlotState
	}{
		"missing":        {want: followerSlotState{}},
		"retaining":      {rows: []map[string]string{{"wal_status": "reserved", "has_restart": "true"}}, want: followerSlotState{Exists: true, Usable: true}},
		"inactive":       {rows: []map[string]string{{"wal_status": "extended", "has_restart": "true"}}, want: followerSlotState{Exists: true, Usable: true}},
		"lost":           {rows: []map[string]string{{"wal_status": "lost", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
		"unreserved":     {rows: []map[string]string{{"wal_status": "unreserved", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
		"no restart":     {rows: []map[string]string{{"wal_status": "reserved", "has_restart": "false"}}, want: followerSlotState{Exists: true}},
		"unknown status": {rows: []map[string]string{{"wal_status": "future_status", "has_restart": "true"}}, want: followerSlotState{Exists: true}},
	} {
		if got := followerSlotStateOf(test.rows); got != test.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A follower's configuration is applied one follower at a time, each run
// limited to that follower and its own pending restarts: the source is never
// in a follower's run, even with the server's restart token (R1-1).
func TestApplyFollowerConfigsRunsEachFollowerAlone(t *testing.T) {
	cfg := followerCfg()
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "app_follower2", Node: "db1", Port: 5435, Size: "small", StatementTimeout: "60s", Follows: "app"})
	var runs []engine.Options
	applyFollowerStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		runs = append(runs, opts)
		return engine.ApplyData{Restarts: []engine.ClusterRestart{{Cluster: opts.DB}}}, nil, nil
	}
	defer func() { applyFollowerStep = engine.Apply }()
	got, err := applyFollowerConfigs(context.Background(), cfg, engine.Runtime{}, "app", engine.Options{Confirm: engine.Confirmation{Server: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app_follower", "app_follower2"}
	if len(runs) != len(want) {
		t.Fatalf("ran %d applies, want one per follower (%d)", len(runs), len(want))
	}
	for i, f := range want {
		if runs[i].DB != f {
			t.Errorf("run %d is for %q, want %q", i, runs[i].DB, f)
		}
		if !slices.Equal(runs[i].Only, []string{steps.ConfigStepID(f)}) {
			t.Errorf("run %d selects %v, want only %s", i, runs[i].Only, steps.ConfigStepID(f))
		}
		if !runs[i].Confirm.Server {
			t.Errorf("run %d lost the restart confirmation", i)
		}
	}
	if got.Failed != "" || len(got.Restarts) != 2 {
		t.Errorf("result %+v, want both followers restarted", got)
	}
}

// A failed follower update stops the run: the next follower is not touched,
// and the failure names the follower it was (Q1).
func TestApplyFollowerConfigsStopsOnAFailure(t *testing.T) {
	cfg := followerCfg()
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "app_follower2", Node: "db1", Port: 5435, Size: "small", StatementTimeout: "60s", Follows: "app"})
	boom := errors.New("postgres-config-v1@app_follower: writing the rendered configuration: disk full")
	var runs []string
	applyFollowerStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		runs = append(runs, opts.DB)
		if opts.DB == "app_follower" {
			return engine.ApplyData{}, nil, boom
		}
		return engine.ApplyData{}, nil, nil
	}
	defer func() { applyFollowerStep = engine.Apply }()
	got, err := applyFollowerConfigs(context.Background(), cfg, engine.Runtime{}, "app", engine.Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the follower's error", err)
	}
	if !slices.Equal(runs, []string{"app_follower"}) {
		t.Errorf("ran %v, want the run to stop at app_follower", runs)
	}
	if got.Failed != "app_follower" {
		t.Errorf("Failed %q, want app_follower", got.Failed)
	}
}

// The dry run shows each follower's own configuration plan, with the floors
// the source change renders and the restart it would take; a database that
// does not follow the resized one stays out (R1-3).
func TestFollowerPlansShowTheFloorChange(t *testing.T) {
	cfg := followerCfg()
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "other", Node: "db1", Port: 5436, Size: "small", StatementTimeout: "60s", Stanza: "other-1a2b3c"})
	rt := engine.Runtime{Host: hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}}
	was, _ := cfg.ClusterByName("app")
	was.Size = "small"
	now, _ := cfg.ClusterByName("app")
	plans, err := followerPlans(context.Background(), cfg, rt, "app", was, now, engine.Options{JournalPath: filepath.Join(t.TempDir(), "journal.json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Follower != "app_follower" {
		t.Fatalf("plans %+v, want app_follower only", plans)
	}
	if plans[0].Restart != engine.RestartRestart {
		t.Errorf("restart %q, want a restart: the floors are postmaster settings", plans[0].Restart)
	}
	var step *engine.PlanStep
	for i := range plans[0].Plan.Steps {
		if id := plans[0].Plan.Steps[i].ID; strings.Contains(id, "other") || id == steps.ConfigStepID("app") {
			t.Errorf("the follower's plan includes %s", id)
		}
		if plans[0].Plan.Steps[i].ID == steps.ConfigStepID("app_follower") {
			step = &plans[0].Plan.Steps[i]
		}
	}
	if step == nil || step.Action != "apply" {
		t.Fatalf("plan %+v lacks an apply of app_follower's configuration", plans[0].Plan.Steps)
	}
	var conf string
	for _, d := range step.Diffs {
		if strings.HasSuffix(d.Path, render.FollowerConfName) {
			conf = d.Diff
		}
	}
	if !strings.Contains(conf, "max_connections = 100") {
		t.Errorf("the plan does not render the source's max_connections floor:\n%s", conf)
	}
}

// resizeFixture saves cfg as the node's cluster.toml and points the node's
// records beside it, so db resize runs against the test's files.
func resizeFixture(t *testing.T, cfg config.Cluster, size string, dryRun bool) (*Env, state.FileStore) {
	t.Helper()
	dir := t.TempDir()
	store := state.FileStore{Path: filepath.Join(dir, "cluster.toml")}
	oldState, oldRecords := followerState, backup.RecordsPath
	followerState = func() state.Store { return store }
	backup.RecordsPath = filepath.Join(dir, "records.json")
	t.Cleanup(func() { followerState, backup.RecordsPath = oldState, oldRecords })
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(raw); err != nil {
		t.Fatal(err)
	}
	env := &Env{Cluster: cfg, Req: request("db.resize", map[string]any{"name": "app", "size": size})}
	env.Req.DryRun = dryRun
	return env, store
}

// A resize that would lower a follower's hot-standby floors is refused before
// it saves or applies anything, in a dry run too (Q1a).
func TestResizeRefusesADecreasedFollowerFloor(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		env, store := resizeFixture(t, followerLifecycleCfg(t, "app_follower"), "small", dryRun)
		_, before, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		applies := 0
		oldResize, oldFollower := applyResizeStep, applyFollowerStep
		applyResizeStep = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
			applies++
			return engine.ApplyData{}, nil, nil
		}
		applyFollowerStep = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
			applies++
			return engine.ApplyData{}, nil, nil
		}
		_, err = handleDBResize(context.Background(), env)
		applyResizeStep, applyFollowerStep = oldResize, oldFollower
		if err == nil || reason(t, err) != "has_followers" {
			t.Fatalf("dry run %v: got %v, want has_followers", dryRun, err)
		}
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || cliErr.Code != output.CodePrecondition || cliErr.ExitCode != output.ExitPrecondition {
			t.Fatalf("dry run %v: %v is not a precondition error", dryRun, err)
		}
		details, _ := cliErr.Details.(map[string]any)
		if details["database"] != "app" {
			t.Errorf("details database %v", details["database"])
		}
		followers, _ := details["followers"].([]string)
		if !slices.Contains(followers, "app_follower") {
			t.Errorf("details followers %v", followers)
		}
		settings, _ := details["settings"].([]steps.FollowerFloorDrop)
		found := false
		for _, s := range settings {
			if s.Follower == "app_follower" && s.Setting == "max_connections" && s.From == 100 && s.To == 30 {
				found = true
			}
		}
		if !found {
			t.Errorf("details settings %v lack app_follower max_connections 100 -> 30", settings)
		}
		if !strings.Contains(cliErr.Message, "hot-standby floors would fall") || !strings.Contains(cliErr.Hint, "vaire db follow app again") {
			t.Errorf("message %q, hint %q", cliErr.Message, cliErr.Hint)
		}
		if applies != 0 {
			t.Errorf("dry run %v: %d applies, want none", dryRun, applies)
		}
		_, after, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Errorf("dry run %v: the refusal saved cluster.toml", dryRun)
		}
	}
}

// Growth and a database without followers pass the floor-drop refusal; a mixed
// change still falls on the setting that would drop (Q1a).
func TestRefuseFollowerFloorDrop(t *testing.T) {
	cfg := followerCfg()
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192}
	large := cfg.Clusters[0] // app at large
	small := large
	small.Size = "small"
	if err := refuseFollowerFloorDrop(cfg, "app", small, large, host); err != nil {
		t.Fatalf("growth: %v", err)
	}
	if err := refuseFollowerFloorDrop(cfg, "app_follower", large, small, host); err != nil {
		t.Fatalf("a database without followers: %v", err)
	}
	if err := refuseFollowerFloorDrop(cfg, "app", large, small, host); err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("shrink: %v, want has_followers", err)
	}
	was, now := large, large
	was.Settings = map[string]string{"max_connections": "200"}
	now.Settings = map[string]string{"max_connections": "100", "max_locks_per_transaction": "512"}
	if err := refuseFollowerFloorDrop(cfg, "app", was, now, host); err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("mixed change: %v, want has_followers", err)
	}
}

// The followers always run before the source, including an identical retry
// after a follower update failed: the size is saved, but the source is not
// applied until every follower succeeded (Q1a).
func TestResizeAppliesFollowersFirstOnRetry(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[0].Size = "small"
	env, store := resizeFixture(t, cfg, "medium", false)
	var runs []string
	followerRuns := 0
	oldResize, oldFollower := applyResizeStep, applyFollowerStep
	applyResizeStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		if opts.DB == "app" {
			runs = append(runs, "source")
			return engine.ApplyData{Restarts: []engine.ClusterRestart{{Cluster: "app"}}}, nil, nil
		}
		runs = append(runs, "kernel")
		return engine.ApplyData{}, nil, nil
	}
	applyFollowerStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		runs = append(runs, opts.DB)
		followerRuns++
		if followerRuns == 1 {
			return engine.ApplyData{}, nil, errors.New("writing app_follower's 60-vaire-follower.conf: disk full")
		}
		return engine.ApplyData{Restarts: []engine.ClusterRestart{{Cluster: opts.DB}}}, nil, nil
	}
	defer func() { applyResizeStep, applyFollowerStep = oldResize, oldFollower }()

	_, err := handleDBResize(context.Background(), env)
	var followerErr *output.CLIError
	if !errors.As(err, &followerErr) || !strings.Contains(followerErr.Error(), "app_follower") || !strings.Contains(followerErr.Hint, "run the same db resize again") {
		t.Fatalf("failed follower update: %v", err)
	}
	if !slices.Equal(runs, []string{"kernel", "app_follower"}) {
		t.Fatalf("first attempt ran %v, want the kernel and the follower only", runs)
	}
	saved, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Clusters[0].Size != "medium" {
		t.Fatalf("the failed attempt saved size %q, want medium", saved.Clusters[0].Size)
	}
	// The identical request sees no size change; the followers still lead.
	env.Cluster = saved
	runs = nil
	res, err := handleDBResize(context.Background(), env)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !slices.Equal(runs, []string{"kernel", "app_follower", "source"}) {
		t.Fatalf("retry ran %v, want the follower before the source", runs)
	}
	data, _ := res.Data.(map[string]any)
	apply, _ := data["apply"].(engine.ApplyData)
	if len(apply.Restarts) != 2 || apply.Restarts[0].Cluster != "app_follower" || apply.Restarts[1].Cluster != "app" {
		t.Errorf("restarts %+v, want app_follower then app", apply.Restarts)
	}
}

// A follower whose restart is already pending counts as restarting in the
// preview, even when its floors do not change, and the preview changes
// neither the journal nor the configuration (R1-3).
func TestFollowerPlansMarkAPendingRestart(t *testing.T) {
	cfg := followerCfg()
	rt := engine.Runtime{Host: hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	id := steps.ConfigStepID("app_follower")
	var inputs []byte
	for _, step := range engine.Expand(steps.Catalog(), cfg) {
		if step.ID() == id {
			var err error
			inputs, err = step.Inputs(cfg, rt)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	journal := &engine.Journal{
		Steps:          map[string]engine.JournalEntry{id: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(id, inputs)}},
		RestartPending: []string{id},
	}
	if err := journal.Save(path); err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp journal.
	if err != nil {
		t.Fatal(err)
	}
	cfgBefore, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := cfg.ClusterByName("app")
	plans, err := followerPlans(context.Background(), cfg, rt, "app", spec, spec, engine.Options{JournalPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Follower != "app_follower" || plans[0].Restart != engine.RestartRestart {
		t.Fatalf("plans %+v, want app_follower restarting", plans)
	}
	if plans[0].Plan.WouldApply != 0 {
		t.Errorf("the preview would apply %d steps, want none", plans[0].Plan.WouldApply)
	}
	journalAfter, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp journal.
	if err != nil {
		t.Fatal(err)
	}
	if string(journalBefore) != string(journalAfter) {
		t.Error("the preview changed the journal")
	}
	cfgAfter, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgBefore) != string(cfgAfter) {
		t.Error("the preview changed the configuration")
	}
}

// A follower whose restart is deferred for lack of its token says its replay
// may pause until it restarts (Q1a).
func TestApplyFollowerConfigsExplainsADeferredRestart(t *testing.T) {
	cfg := followerCfg()
	old := applyFollowerStep
	applyFollowerStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		id := steps.ConfigStepID(opts.DB)
		return engine.ApplyData{RestartPending: []string{id}, ConfirmTokens: []string{opts.DB + "/restart"}},
			[]output.Warning{{Code: "restart_pending", Message: "a restart is pending for " + id + "; re-run apply with --confirm " + opts.DB + "/restart"}}, nil
	}
	defer func() { applyFollowerStep = old }()
	got, err := applyFollowerConfigs(context.Background(), cfg, engine.Runtime{}, "app", engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0].Message, "app_follower may pause replay until it restarts") ||
		!strings.Contains(got.Warnings[0].Message, "PostgreSQL 18 pauses recovery") {
		t.Fatalf("warnings %+v, want the replay pause", got.Warnings)
	}
}

// A retried resize whose earlier follower write failed has the same old and
// new entry, and no restart pending yet; the follower's file on disk still
// lacks the new floors, so the preview still shows its restart (R1-3). This
// host has no follower file, which is the failed write's case.
func TestFollowerPlansShowARetrysRestart(t *testing.T) {
	cfg := followerCfg()
	rt := engine.Runtime{Host: hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}}
	spec, _ := cfg.ClusterByName("app")
	plans, err := followerPlans(context.Background(), cfg, rt, "app", spec, spec, engine.Options{JournalPath: filepath.Join(t.TempDir(), "journal.json")})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Follower != "app_follower" || plans[0].Restart != engine.RestartRestart {
		t.Fatalf("plans %+v, want app_follower restarting", plans)
	}
	if len(plans[0].Plan.RestartPending) != 0 {
		t.Fatalf("restart pending %v, want none: the restart comes from the file", plans[0].Plan.RestartPending)
	}
}
