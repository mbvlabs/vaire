package node

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/state"
)

func TestDumpRPCSurfaceRemoved(t *testing.T) {
	for name := range methods() {
		if strings.HasPrefix(name, "dump.") {
			t.Fatalf("dump RPC %q is still registered", name)
		}
	}
}

func TestDumpRestoreKindRefusedBeforeEffects(t *testing.T) {
	useTempExtensionRecords(t)

	cfg := config.DefaultCluster()

	env := &Env{Cluster: cfg, Req: request("restore.start", map[string]any{"database": "app", "kind": "dump"})}
	if _, err := handleRestoreStart(context.Background(), env); err == nil || reasonOf(t, err) != "restore_kind_unsupported" {
		t.Fatalf("dump restore request = %v", err)
	}

	if recs, err := backup.LoadRecords(); err != nil || len(recs.Restores) != 0 {
		t.Fatalf("refused request wrote a restore: %+v %v", recs.Restores, err)
	}

	if _, err := jobRestore(context.Background(), &jobEnv{Params: json.RawMessage(`{"database":"app","kind":"dump"}`)}); err == nil || reasonOf(t, err) != "restore_kind_unsupported" {
		t.Fatalf("dump restore job = %v", err)
	}

	record := backup.Restore{ID: "dump-op", Database: "app", Kind: "dump", Target: "dump1", Phase: backup.PhaseReady}

	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = record; return nil }))

	if _, err := preparePromotion(context.Background(), cfg, "app", engine.Runtime{}, nil); err == nil || reasonOf(t, err) != "restore_kind_unsupported" {
		t.Fatalf("dump promote = %v", err)
	}

	if recs, _ := backup.LoadRecords(); !reflect.DeepEqual(recs.Restores["app"], record) {
		t.Fatalf("refused promote changed the record: %+v", recs.Restores["app"])
	}

	if _, err := handleRestoreAbort(context.Background(), &Env{Cluster: cfg, Req: request("restore.abort", map[string]any{"database": "app"})}); err == nil || reasonOf(t, err) != "restore_kind_unsupported" {
		t.Fatalf("dump abort = %v", err)
	}

	if recs, _ := backup.LoadRecords(); !reflect.DeepEqual(recs.Restores["app"], record) {
		t.Fatalf("refused abort changed the record: %+v", recs.Restores["app"])
	}

	failed := record
	failed.Phase, failed.MaterialisationPending = backup.PhaseFailed, true

	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = failed; return nil }))

	if err := clearFailedRestore(context.Background(), cfg, "app"); err == nil || reasonOf(t, err) != "restore_kind_unsupported" {
		t.Fatalf("dump cleanup = %v", err)
	}

	if recs, _ := backup.LoadRecords(); !reflect.DeepEqual(recs.Restores["app"], failed) {
		t.Fatalf("refused cleanup changed the record: %+v", recs.Restores["app"])
	}
}

func TestDBDropReportsNoSnapshot(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Backup.Provider, cfg.Backup.Bucket = "s3", "bucket"
	spec := config.ClusterSpec{Name: "app", Stanza: "app-abc123"}

	warnings := dropSnapshotWarning(cfg, "app", true, true)
	if len(warnings) != 1 || warnings[0].Code != "no_backup_before_drop" || !strings.Contains(warnings[0].Message, "no snapshot") {
		t.Fatalf("configured drop warning = %+v", warnings)
	}

	result := dropResult("app", spec, []string{"app_owner"}, true, warnings)

	data, _ := result.Data.(map[string]any)
	if _, ok := data["final_dump"]; ok {
		t.Fatalf("drop still reports an exported dump: %+v", data)
	}

	if data["stanza_kept"] != spec.Stanza {
		t.Fatalf("drop lost the kept stanza: %+v", data)
	}

	if len(dropSnapshotWarning(cfg, "app", false, false)) != 0 {
		t.Fatal("a missing database warned about a snapshot")
	}

	cfg.Backup.Provider = ""
	if got := dropSnapshotWarning(cfg, "app", true, false); len(got) != 1 || !strings.Contains(got[0].Message, "no copy") {
		t.Fatalf("unconfigured drop warning = %+v", got)
	}

	cfg.Backup.Provider = "s3"
	if got := dropSnapshotWarning(cfg, "follower", true, false); len(got) != 1 || !strings.Contains(got[0].Message, "no independent physical backup stanza is kept") {
		t.Fatalf("drop without its own stanza = %+v", got)
	}
}

// A drop with backups configured must not export a final dump or prune remote
// objects: with the bucket keys absent, any attempt would fail before the drop.
func TestDBDropDoesNotExportOrPrune(t *testing.T) {
	useTempExtensionRecords(t)
	_, root := materialisationFixture(t)
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Backup.Provider, cfg.Backup.Bucket = "s3", "bucket"
	cfg.Clusters = []config.ClusterSpec{{Name: "dropme", Size: "small", Port: 5433}}
	store := &dropConfigStore{cfg: cfg}
	env := &Env{Cluster: cfg, Req: request("db.drop", map[string]any{"name": "dropme"})}
	env.Req.Confirm = "dropme"

	oldStore, oldExists, oldDatabaseExists, oldCluster := dbDropStore, dbDropClusterExists, dbDropDatabaseExists, dbDropCluster
	oldRoles, oldLeftovers, oldJournal := dbDropRoles, dbDropLeftovers, dbDropForgetJournal
	oldOther, oldApply, oldRouting := dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting
	oldMetrics, oldClear := dbDropMetrics, dbDropClearDeferred

	t.Cleanup(func() {
		dbDropStore, dbDropClusterExists, dbDropDatabaseExists, dbDropCluster = oldStore, oldExists, oldDatabaseExists, oldCluster
		dbDropRoles, dbDropLeftovers, dbDropForgetJournal = oldRoles, oldLeftovers, oldJournal
		dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting = oldOther, oldApply, oldRouting
		dbDropMetrics, dbDropClearDeferred = oldMetrics, oldClear
	})

	dbDropStore = func() state.Store { return store }
	dbDropClusterExists = func(context.Context, string) bool { return true }
	dbDropDatabaseExists = func(context.Context, pg.Local, string, bool, bool) bool { return true }
	dbDropCluster = func(context.Context, *Env, string) error { return nil }
	dbDropRoles = func(context.Context, sqlRunner, string) ([]string, error) { return []string{"dropme_owner"}, nil }
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

	result, err := handleDBDrop(context.Background(), env)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}

	if len(result.Warnings) != 1 || result.Warnings[0].Code != "no_backup_before_drop" {
		t.Fatalf("drop warnings = %+v", result.Warnings)
	}

	data, _ := result.Data.(map[string]any)
	if _, ok := data["final_dump"]; ok || data["dropped"] != "dropme" {
		t.Fatalf("drop result = %+v", data)
	}
}
