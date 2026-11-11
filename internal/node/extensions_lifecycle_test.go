package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/steps"
)

// failTargetPreflight replaces the direct-lifecycle preflight with one that
// fails, so the test can prove no durable record or host effect happens after
// it. The production default is restored by t.Cleanup.
func failTargetPreflight(t *testing.T) error {
	t.Helper()
	old := prepareTargetExtensions
	marker := errors.New("target extension preflight failed")
	prepareTargetExtensions = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime, engine.Reporter) error {
		return marker
	}
	t.Cleanup(func() { prepareTargetExtensions = old })
	return marker
}

func useTempRecords(t *testing.T) {
	t.Helper()
	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
}

// TestTuneApplyPreflightFailureStopsBeforeConfigWrite runs the real
// tune.apply handler with a temporary journal and a target whose extension
// definition cannot resolve. The kernel step is already complete, so the
// handler reaches the config step and stops in its input/preflight phase
// without writing a generated file.
func TestTuneApplyPreflightFailureStopsBeforeConfigWrite(t *testing.T) {
	oldJournal, oldDetect := tuneJournalPath, detectHost
	tuneJournalPath = filepath.Join(t.TempDir(), "journal.json")
	detectHost = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	t.Cleanup(func() {
		tuneJournalPath, detectHost = oldJournal, oldDetect
	})
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "tuneapplytest", Size: "small", Port: 5433,
		Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: "bogus"},
	}
	kernelID := steps.TuneStepIDs()[0]
	var kernel engine.Step
	for _, step := range engine.Expand(steps.Catalog(), cfg) {
		if step.ID() == kernelID {
			kernel = step
			break
		}
	}
	if kernel == nil {
		t.Fatal("kernel step not found")
	}
	inputs, err := kernel.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		kernelID: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(kernelID, inputs)},
	}}
	if err := j.Save(tuneJournalPath); err != nil {
		t.Fatal(err)
	}

	_, err = handleTuneApply(context.Background(), &Env{Req: request("tune.apply", nil), Cluster: cfg})
	if err == nil || !strings.Contains(err.Error(), `source "bogus"`) {
		t.Fatalf("tune.apply preflight failure = %v", err)
	}
	if _, statErr := os.Stat(steps.PGConfDir("tuneapplytest")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tune.apply wrote a config file before the prerequisite failed: %v", statErr)
	}
}

func TestPrepareTargetExtensionsDefaultResolvesTheTarget(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{"custom": {Source: "bogus"}}
	err := prepareTargetExtensionsDefault(context.Background(), cfg, cfg.Clusters[0], engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), `source "bogus"`) {
		t.Fatalf("default target preflight = %v", err)
	}
}

func TestPromotePreflightFailureLeavesNoRecordOrEffect(t *testing.T) {
	useTempRecords(t)
	marker := failTargetPreflight(t)
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small"}}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{
			ID: "r1", Database: "app", Target: backup.TargetLatest, Kind: backup.TargetLatest,
			Phase: backup.PhaseReady, StartedAt: "2026-01-01T00:00:00Z", Port: 6001,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {})
	if !errors.Is(err, marker) {
		t.Fatalf("promote preflight failure = %v", err)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if recs.Restores["app"].Aside != "" {
		t.Fatalf("promote recorded an aside before the preflight: %+v", recs.Restores["app"])
	}
	if len(recs.Leftovers) != 0 || len(recs.Undoing) != 0 {
		t.Fatalf("promote changed lifecycle records before the preflight: %+v", recs)
	}
	entries, err := os.ReadDir(cfg.Server.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("promote touched the data root before the preflight: %v", entries)
	}
}

func TestUndoPreflightFailureLeavesNoRecordOrEffect(t *testing.T) {
	useTempRecords(t)
	marker := failTargetPreflight(t)
	oldLock := lockForCutover
	lockForCutover = func(context.Context, *Env, string, string) (func(), error) { return func() {}, nil }
	oldDetect := detectHost
	detectHost = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	t.Cleanup(func() {
		lockForCutover = oldLock
		detectHost = oldDetect
	})
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small"}}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Leftovers = append(s.Leftovers, backup.Leftover{
			Database: "app", Name: "app__pre_restore_20260101T000000", RestoreID: "r1", Port: 6001,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	req := request("restore.undo", map[string]any{"database": "app"})
	req.Confirm = "app/restore"
	env := &Env{Req: req, Cluster: cfg}
	_, err := handleRestoreUndo(context.Background(), env)
	if !errors.Is(err, marker) {
		t.Fatalf("undo preflight failure = %v", err)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs.Undoing) != 0 {
		t.Fatalf("undo recorded its plan before the preflight: %+v", recs.Undoing)
	}
	if len(recs.Leftovers) != 1 || recs.Leftovers[0].Name != "app__pre_restore_20260101T000000" {
		t.Fatalf("undo changed leftovers before the preflight: %+v", recs.Leftovers)
	}
	entries, err := os.ReadDir(cfg.Server.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("undo touched the data root before the preflight: %v", entries)
	}
}

func TestJobUnfollowPreflightFailureLeavesNoRecordOrEffect(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{}).install(t)
	marker := failTargetPreflight(t)

	_, err := jobUnfollow(context.Background(), unfollowEnv(cfg, "app_follower"))
	if !errors.Is(err, marker) {
		t.Fatalf("unfollow preflight failure = %v", err)
	}
	if calls := stub.stages(); len(calls) != 0 {
		t.Fatalf("unfollow ran server effects before the preflight: %v", calls)
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Fatalf("unfollow recorded its transition before the preflight: %+v", recs)
	}
	spec, ok := node.config().ClusterByName("app_follower")
	if !ok || spec.Follows != "app" || spec.Stanza != "" {
		t.Fatalf("unfollow changed cluster.toml before the preflight: %+v", spec)
	}
	entries, err := os.ReadDir(cfg.Server.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unfollow touched the data root before the preflight: %v", entries)
	}
}
