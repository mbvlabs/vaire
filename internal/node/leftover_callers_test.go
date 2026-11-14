package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

func leftoverCallerFixture(t *testing.T) (config.Cluster, backup.Leftover, *int) {
	t.Helper()
	_, root := materialisationFixture(t)
	ports := filepath.Join(materialisationAncestorBase, "ports")
	for _, dir := range []string{root, ports} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldPath, oldRes, oldProbe, oldDrop := backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop
	t.Cleanup(func() {
		backup.RecordsPath, legacyLeftoverReservation, legacyLeftoverEffectiveSetting, restoreCleanupDrop = oldPath, oldRes, oldProbe, oldDrop
	})
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	l := backup.Leftover{Database: "app", Name: "app__pre_restore_20260101T010203", RestoreID: "job", Port: config.TemporaryPortMin, Stanza: "app-abcdef"}
	data := filepath.Join(root, l.Name)
	conf := filepath.Join(materialisationConfigRoot, l.Name)
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(conf, 0o755); err != nil { //nolint:gosec // registration fixture
		t.Fatal(err)
	}
	raw := fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", data, l.Name, l.Port)
	if err := os.WriteFile(filepath.Join(conf, "postgresql.conf"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ports, l.Name), []byte(fmt.Sprintf("%d\n", l.Port)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error { s.Leftovers = []backup.Leftover{l}; return nil }); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: l.Stanza, Size: "small"}}
	legacyLeftoverReservation = func(ctx context.Context, name string, port int, effect func() error) error {
		return backup.CleanupCurrentLegacyReservationWithHost(ctx, ports, materialisationRecordUID, name, port, effect)
	}
	legacyLeftoverEffectiveSetting = func(_ string, setting string) (string, error) {
		switch setting {
		case "data_directory":
			return data, nil
		case "cluster_name":
			return "18/" + l.Name, nil
		case "port":
			return fmt.Sprint(l.Port), nil
		}
		return "", errors.New("unexpected setting")
	}
	drops := new(int)
	restoreCleanupDrop = func(context.Context, *Env, string) error { *drops++; return nil }
	return cfg, l, drops
}

func TestRecoverScaffoldCleanupLease(t *testing.T) {
	for _, mode := range []string{"success", "copied", "finished", "install", "current-install", "job", "params", "no-db-lock", "leftover", "port", "stanza", "path", "drop-failure"} {
		t.Run(mode, func(t *testing.T) {
			cfg, l, drops := leftoverCallerFixture(t)
			env := &jobEnv{ID: "job", Job: "recover", OperationID: "operation", installID: "install", Params: json.RawMessage(`{"database":"app"}`), Cluster: cfg}
			acquired := 0
			locks := jobLockers{
				node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
					acquired++
					return func() {}, nil
				},
				heavyWait: func(context.Context, string, string, time.Duration) (engine.Release, error) {
					acquired++
					return func() {}, nil
				},
				cluster: func(_ context.Context, name string, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
					acquired++
					if name == "app" && mode != engine.LockShared || name == "app__restoring" && mode != engine.LockExclusive {
						t.Fatalf("wrong lock: %s %s", name, mode)
					}
					return func() {}, nil
				},
			}
			lease, busy, err := takeJobLocks(context.Background(), env, locks)
			if err != nil || busy != nil {
				t.Fatalf("lease: %v %v", busy, err)
			}
			env.lease = lease
			t.Cleanup(lease.finish)
			lease.releaseHeavyIO()
			count := acquired
			switch mode {
			case "copied":
				copyEnv := *env
				env = &copyEnv
			case "finished":
				lease.finish()
			case "install":
				env.installID = "foreign"
			case "current-install":
				followHostInstance = func() (string, error) { return "foreign", nil }
			case "job":
				env.Job = "restore"
			case "params":
				env.Params = json.RawMessage(`{"database":"other"}`)
			case "no-db-lock":
				delete(lease.targets, "app")
			case "leftover":
				if err := backup.UpdateRecords(func(s *backup.Records) error { s.Leftovers[0].RestoreID = "foreign"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "port":
				l.Port++
			case "stanza":
				l.Stanza = "foreign"
			case "path":
				if err := os.Rename(filepath.Join(cfg.Server.DataRoot, l.Name), filepath.Join(cfg.Server.DataRoot, l.Name+"-old")); err != nil {
					t.Fatal(err)
				}
			case "drop-failure":
				restoreCleanupDrop = func(context.Context, *Env, string) error { *drops++; return errors.New("drop failed") }
			}
			result := map[string]any{}
			recoverScaffoldResult(context.Background(), env, cfg, "app", l.Name, l.Port, l.Stanza, result)
			_, warned := result["scaffold_warning"]
			if (mode != "success") != warned {
				t.Fatalf("unexpected warning: %+v", result)
			}
			if acquired != count {
				t.Fatal("reacquired job lock")
			}
			s, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" && (len(s.Leftovers) != 0 || *drops != 1) || mode != "success" && len(s.Leftovers) != 1 {
				t.Fatalf("unexpected state: %+v drops=%d", s.Leftovers, *drops)
			}
			if mode == "drop-failure" {
				if _, err := os.Stat(filepath.Join(materialisationAncestorBase, "ports", l.Name)); err != nil {
					t.Fatalf("drop failure lost reservation: %v", err)
				}
			}
		})
	}
}

func TestDBDropUsesVerifiedLeftovers(t *testing.T) {
	for _, mode := range []string{"success", "foreign", "changed", "undo", "undo-halfway", "undo-done", "drill", "retained"} {
		t.Run(mode, func(t *testing.T) {
			cfg, l, drops := leftoverCallerFixture(t)
			oldStore, oldExists, oldJournal := dbDropStore, dbDropClusterExists, dbDropForgetJournal
			oldOther, oldApply, oldRouting, oldMetrics, oldClear, oldDetect := dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred, detectHost
			t.Cleanup(func() {
				dbDropStore, dbDropClusterExists, dbDropForgetJournal = oldStore, oldExists, oldJournal
				dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred, detectHost = oldOther, oldApply, oldRouting, oldMetrics, oldClear, oldDetect
			})
			store := &followerDropStore{cfg: cfg}
			dbDropStore = func() state.Store { return store }
			dbDropClusterExists = func(context.Context, string) bool { return false }
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
			detectHost = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
			switch mode {
			case "foreign":
				if err := os.Remove(filepath.Join(materialisationConfigRoot, l.Name, "postgresql.conf")); err != nil {
					t.Fatal(err)
				}
			case "undo", "undo-halfway", "undo-done":
				aside := "app__pre_restore_20260101T020304"
				if mode != "undo" {
					if err := os.Mkdir(filepath.Join(cfg.Server.DataRoot, aside), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "undo-done" {
					if err := os.Rename(filepath.Join(cfg.Server.DataRoot, l.Name), filepath.Join(cfg.Server.DataRoot, "app")); err != nil {
						t.Fatal(err)
					}
				}
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					s.Undoing["app"] = backup.Undo{From: l, Aside: aside}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "drill":
				if err := backup.UpdateRecords(func(s *backup.Records) error { s.Drilling["app"] = "app__drill"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "retained":
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					s.Restores["app"] = backup.Restore{ID: "job", Database: "app", MaterialisationPending: true}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "changed":
				restoreCleanupDrop = func(context.Context, *Env, string) error {
					*drops++
					return backup.UpdateRecords(func(s *backup.Records) error { s.Leftovers[0].Stanza = "changed"; return nil })
				}
			}
			_, err := handleDBDrop(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: json.RawMessage(`{"name":"app"}`), Confirm: "app"}})
			if (mode == "success") != (err == nil) {
				t.Fatalf("drop: %v", err)
			}
			s, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" && (len(s.Leftovers) != 0 || *drops != 1) || mode != "success" && len(s.Leftovers) != 1 {
				t.Fatalf("unexpected state: %+v drops=%d", s.Leftovers, *drops)
			}
			if mode != "success" && mode != "changed" && *drops != 0 {
				t.Fatalf("unsafe drop executed: %d", *drops)
			}
		})
	}
}
