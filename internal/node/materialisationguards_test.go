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

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestMaterialisationDispatchPrecedesAwaiting(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldRecords })
	raw, err := json.Marshal(backup.Records{AwaitingRecovery: map[string]backup.Awaiting{o.Target: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup.RecordsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target}}
	old := loadGuardCluster
	loadGuardCluster = func() (config.Cluster, error) { return cfg, nil }
	t.Cleanup(func() { loadGuardCluster = old })
	t.Cleanup(masterbackup.SetTestRoot(t.TempDir()))
	withServeActivity(t, &fakeActivityStore{})
	req := matchingBuildRequest("apply")
	req.Params = json.RawMessage(`{"db":"target"}`)
	out, code := serveRequest(t, req)
	if code != output.ExitPrecondition || !strings.Contains(out, "materialisation_incomplete") ||
		strings.Contains(out, "awaiting_recovery") {
		t.Fatalf("wrong precedence: %d %s", code, out)
	}
}

func TestMaterialisationPublicApplyPreflight(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.Cluster{}
	cfg.Server.DataRoot = root
	for _, raw := range []string{
		`{"db":"target"}`, `{}`, `{"steps":["postgres-cluster-v1"]}`,
		`{"steps":["postgres-cluster-v1@target"]}`,
	} {
		for _, dry := range []bool{false, true} {
			env := &Env{Cluster: cfg, Req: rpc.Request{Params: json.RawMessage(raw), DryRun: dry}}
			_, err := handleApply(context.Background(), env)
			var e *output.CLIError
			if !errors.As(err, &e) || e.ExitCode != output.ExitPrecondition ||
				e.Details.(map[string]any)["reason"] != "materialisation_incomplete" {
				t.Fatalf("params %s dry=%v: %v", raw, dry, err)
			}
		}
	}
}

func TestMaterialisationRequestMutationClasses(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	for _, method := range []string{
		"apply", "db.create", "db.copy", "db.resize", "db.recover", "db.credentials",
		"db.write-access", "db.fork", "db.follow", "db.unfollow", "backup.now",
		"backup.verify", "restore.start", "restore.promote", "restore.undo", "restore.drill",
		"dump.create", "maint.run", "maint.kill", "queries.reset", "sql",
	} {
		for _, field := range []string{"db", "database", "name", "source"} {
			if method == "apply" && field != "db" {
				continue
			}
			raw := json.RawMessage(`{"` + field + `":"target"}`)
			err := refuseMaterialisationRequest(cfg, rpc.Request{Method: method, Params: raw})
			var e *output.CLIError
			if !errors.As(err, &e) || e.ExitCode != output.ExitPrecondition ||
				e.Details.(map[string]any)["reason"] != "materialisation_incomplete" {
				t.Errorf("%s %s: %v", method, field, err)
			}
		}
	}
	for _, method := range []string{"db.drop", "db.list", "logs", "status", "db.access.show"} {
		if err := refuseMaterialisationRequest(cfg, rpc.Request{Method: method, Params: json.RawMessage(`{"name":"target"}`)}); err != nil {
			t.Errorf("%s should remain available: %v", method, err)
		}
	}
}

func TestMaterialisationReloadAfterWaiting(t *testing.T) {
	o, root := materialisationFixture(t)
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target}}
	current := cfg
	old := loadGuardCluster
	loadGuardCluster = func() (config.Cluster, error) { return current, nil }
	t.Cleanup(func() { loadGuardCluster = old })
	req := rpc.Request{Method: "apply", Params: json.RawMessage(`{"db":"target"}`)}
	if _, err := reloadMaterialisationConfig(req); err != nil {
		t.Fatal(err)
	}
	// Another operation creates the marker while the apply waits for locks.
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadMaterialisationConfig(req); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("new marker not rechecked: %v", err)
	}
	// An owned drop completes while the apply waits: both marker and target
	// are gone. The old snapshot must not reach the engine.
	path, _ := materialisationPath(o.Target)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	current.Clusters = nil
	fresh, err := reloadMaterialisationConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Plan(context.Background(), steps.Catalog(), fresh, engine.Runtime{},
		engine.Options{DB: o.Target}); err == nil {
		t.Fatal("stale target was accepted after drop")
	}
	loadGuardCluster = func() (config.Cluster, error) { return config.Cluster{}, errors.New("read failed") }
	if _, err := reloadMaterialisationConfig(req); err == nil {
		t.Fatal("accepted an unreadable current configuration")
	}
}

func TestMaterialisationSharedRenderExcludesPartial(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Backup.Provider, cfg.Backup.Bucket = "r2", "bucket"
	cfg.Backup.Database.FullSchedule = "daily"
	cfg.Clusters = []config.ClusterSpec{
		{Name: o.Target, Stanza: "target-abcdef", Port: 5433, Size: "small"},
		{Name: "other", Stanza: "other-abcdef", Port: 5434, Size: "small"},
	}
	type key struct{}
	ownerCtx := context.WithValue(context.Background(), key{}, true)
	old := materialisationOwner
	materialisationOwner = func(ctx context.Context, got materialisationOwnership) bool {
		return ctx.Value(key{}) == true && got == o
	}
	t.Cleanup(func() { materialisationOwner = old })
	for _, tc := range []struct {
		name string
		ctx  context.Context
		opts engine.Options
	}{
		{"other", context.Background(), engine.Options{DB: "other"}},
		{"shared-owner", ownerCtx, engine.Options{}},
		{"forged-recovering", context.Background(), engine.Options{Recovering: o.Target}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, err := materialisationGuardView(tc.ctx, cfg, tc.opts)
			if err != nil || len(view.Awaiting) != 1 || view.Awaiting[0].Name != o.Target {
				t.Fatalf("budget view: %+v %v", view, err)
			}
			host := hostinfo.Info{VCPU: 16, DataDisk: hostinfo.Disk{SizeGiB: 400}}
			withBoth := steps.TuningFor(cfg, cfg.Clusters[1], host)
			if got := steps.TuningFor(view, view.Clusters[0], host); !reflect.DeepEqual(got, withBoth) {
				t.Fatal("incomplete target was omitted from tuning budget")
			}
			alone := view
			alone.Awaiting = nil
			if reflect.DeepEqual(steps.TuningFor(alone, alone.Clusters[0], host), withBoth) {
				t.Fatal("budget test did not distinguish one target from two")
			}
			var pooler, timers []engine.RenderedFile
			for _, step := range steps.Catalog() {
				renderer, ok := step.(engine.FileRenderer)
				if !ok {
					continue
				}
				switch step.ID() {
				case "pgbouncer-v1":
					pooler = renderer.RenderedFiles(view, engine.Runtime{})
				case "timers-v1":
					timers = renderer.RenderedFiles(view, engine.Runtime{})
				}
			}
			if len(pooler) == 0 || len(timers) == 0 {
				t.Fatal("expected actual pooler and timer renders")
			}
			var poolText, timerPaths string
			for _, f := range pooler {
				poolText += string(f.Content)
			}
			for _, f := range timers {
				timerPaths += f.Path + "\n"
			}
			if strings.Contains(poolText, "\ntarget =") || strings.Contains(poolText, "\ntarget__auth =") ||
				!strings.Contains(poolText, "\nother =") {
				t.Fatalf("unsafe pooler render: %s", poolText)
			}
			if strings.Contains(timerPaths, "@target.timer") || !strings.Contains(timerPaths, "@other.timer") {
				t.Fatalf("unsafe timer render: %s", timerPaths)
			}
		})
	}
}

func TestMaterialisationGuardViews(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.Cluster{}
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: o.Target}, {Name: "other"}}
	cfg.Awaiting = []config.ClusterSpec{{Name: "prior"}}
	if err := guardMaterialisationApply(cfg, o.Target); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("target: %v", err)
	}
	if err := guardMaterialisationApply(cfg, ""); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("all: %v", err)
	}
	if err := guardMaterialisationApply(cfg, "other"); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []engine.Options{{}, {DB: "other"}, {DB: o.Target, Recovering: o.Target}} {
		view, err := materialisationGuardView(context.Background(), cfg, opts)
		if err != nil || len(view.Clusters) != 1 || view.Clusters[0].Name != "other" || len(view.Awaiting) != 2 {
			t.Fatalf("view %+v: %+v, %v", opts, view, err)
		}
	}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "private")
	old := materialisationOwner
	materialisationOwner = func(c context.Context, got materialisationOwnership) bool {
		return c.Value(key{}) == "private" && got == o
	}
	t.Cleanup(func() { materialisationOwner = old })
	view, err := materialisationGuardView(ctx, cfg, engine.Options{DB: o.Target})
	if err != nil || len(view.Clusters) != 2 {
		t.Fatalf("owned target view: %+v %v", view, err)
	}
	view, err = materialisationGuardView(ctx, cfg, engine.Options{})
	if err != nil || len(view.Clusters) != 1 {
		t.Fatalf("owned shared view: %+v %v", view, err)
	}
}

func TestMaterialisationGuardsRejectBadRecords(t *testing.T) {
	for _, kind := range []string{"mode", "corrupt", "foreign", "replaced", "read"} {
		t.Run(kind, func(t *testing.T) {
			o, root := materialisationFixture(t)
			if _, err := createMaterialisation(o, root); err != nil {
				t.Fatal(err)
			}
			p, _ := materialisationPath(o.Target)
			switch kind {
			case "mode":
				if err := os.Chmod(p, 0o644); err != nil { //nolint:gosec // intentionally unsafe record
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				followHostInstance = func() (string, error) { return "foreign", nil }
			case "replaced":
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(materialisationStateDir, "other"), p); err != nil {
					t.Fatal(err)
				}
			case "read":
				materialisationStateDir = filepath.Join(materialisationStateDir, "missing")
			}
			cfg := config.Cluster{}
			cfg.Server.DataRoot = root
			for _, db := range []string{"", o.Target} {
				err := guardMaterialisationApply(cfg, db)
				if output.ExitCode(err) != output.ExitPrecondition {
					t.Fatalf("%s: %v", db, err)
				}
			}
			_, err := materialisationGuardView(context.Background(), cfg, engine.Options{})
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe view error: %v", err)
			}
		})
	}
}
