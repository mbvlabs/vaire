package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestConfigSetFollowerAllRecoveryFloors(t *testing.T) {
	for _, setting := range steps.FloorSettings {
		for _, full := range []bool{false, true} {
			for _, dry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/full=%v/dry=%v", setting, full, dry), func(t *testing.T) {
					cfg := followerLifecycleCfg(t, "app_follower")
					cfg.Clusters[0].Settings = map[string]string{setting: "100"}
					cfg.Clusters[1].Settings = map[string]string{setting: "200"}
					env, store := resizeFixture(t, cfg, "small", dry)
					oldStore := configSetStore
					configSetStore = func() state.FileStore { return store }
					t.Cleanup(func() { configSetStore = oldStore })
					rev, err := store.Revision()
					if err != nil {
						t.Fatal(err)
					}
					_, before, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					params := map[string]any{"base_revision": rev}
					if full {
						cfg.Clusters[1].Settings = map[string]string{setting: "150"}
						raw, err := cfg.Encode()
						if err != nil {
							t.Fatal(err)
						}
						params["toml"] = string(raw)
					} else {
						params["set"] = map[string]string{"clusters.app_follower.settings." + setting: "150"}
					}
					env.Req = request("config.set", params)
					env.Req.DryRun = dry
					_, err = handleConfigSet(context.Background(), env)
					if err == nil || reason(t, err) != "follower_recovery_floor_decrease" {
						t.Fatalf("got %v", err)
					}
					var cli *output.CLIError
					if !errors.As(err, &cli) || cli.Code != output.CodePrecondition || cli.ExitCode != output.ExitPrecondition {
						t.Fatalf("wrong error type: %v", err)
					}
					details, _ := cli.Details.(map[string]any)
					drops, _ := details["settings"].([]recoveryFloorDecrease)
					if details["database"] != "app_follower" || details["source"] != "app" ||
						!slices.Contains(drops, recoveryFloorDecrease{Setting: setting, Before: 200, After: 150}) {
						t.Fatalf("wrong details: %+v", details)
					}
					_, after, err := store.Load()
					if err != nil || string(before) != string(after) {
						t.Fatalf("saved on refusal: %v", err)
					}
				})
			}
		}
	}
}

func TestConfigSetSourceAllRecoveryFloorsMasked(t *testing.T) {
	for _, setting := range steps.FloorSettings {
		for _, full := range []bool{false, true} {
			for _, dry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/full=%v/dry=%v", setting, full, dry), func(t *testing.T) {
					cfg := followerLifecycleCfg(t, "app_follower")
					cfg.Clusters[0].Settings = map[string]string{setting: "200"}
					cfg.Clusters[1].Settings = map[string]string{setting: "300"}
					env, store := resizeFixture(t, cfg, "small", dry)
					oldStore := configSetStore
					configSetStore = func() state.FileStore { return store }
					t.Cleanup(func() { configSetStore = oldStore })
					rev, err := store.Revision()
					if err != nil {
						t.Fatal(err)
					}
					_, before, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					params := map[string]any{"base_revision": rev}
					if full {
						cfg.Clusters[0].Settings = map[string]string{setting: "150"}
						raw, err := cfg.Encode()
						if err != nil {
							t.Fatal(err)
						}
						params["toml"] = string(raw)
					} else {
						params["set"] = map[string]string{"clusters.app.settings." + setting: "150"}
					}
					env.Req = request("config.set", params)
					env.Req.DryRun = dry
					_, err = handleConfigSet(context.Background(), env)
					if err == nil || reason(t, err) != "has_followers" {
						t.Fatalf("got %v", err)
					}
					var cli *output.CLIError
					if !errors.As(err, &cli) || cli.Code != output.CodePrecondition || cli.ExitCode != output.ExitPrecondition {
						t.Fatalf("wrong error type: %v", err)
					}
					details, _ := cli.Details.(map[string]any)
					drops, _ := details["settings"].([]recoveryFloorDecrease)
					names, _ := details["followers"].([]string)
					if details["database"] != "app" || !slices.Contains(names, "app_follower") ||
						!slices.Contains(drops, recoveryFloorDecrease{Setting: setting, Before: 200, After: 150}) {
						t.Fatalf("wrong details: %+v", details)
					}
					_, after, err := store.Load()
					if err != nil || string(before) != string(after) {
						t.Fatalf("saved on refusal: %v", err)
					}
				})
			}
		}
	}
}

func TestConfigSetUsesPostgresFloorParsing(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		want                string
	}{
		{"octal-to-hex-drop", "0310", "0x96", "has_followers"},
		{"floating-point-drop", "2e2", "149.6", "has_followers"},
		{"invalid-new", "200", "150MB", "parse"},
		{"invalid-old", "not-a-number", "200", "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			cfg.Postgres.Settings = map[string]string{"max_worker_processes": tc.before}
			cfg.Clusters[1].Settings = map[string]string{"max_worker_processes": "300"}
			env, store := resizeFixture(t, cfg, "small", true)
			oldStore := configSetStore
			configSetStore = func() state.FileStore { return store }
			t.Cleanup(func() { configSetStore = oldStore })
			revision, err := store.Revision()
			if err != nil {
				t.Fatal(err)
			}
			_, before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			env.Req = request("config.set", map[string]any{
				"base_revision": revision,
				"set":           map[string]string{"postgres.settings.max_worker_processes": tc.after},
			})
			env.Req.DryRun = true
			_, err = handleConfigSet(context.Background(), env)
			switch tc.want {
			case "parse":
				if err == nil || !strings.Contains(err.Error(), "max_worker_processes") {
					t.Fatalf("expected invalid floor error, got %v", err)
				}
			default:
				if err == nil || reason(t, err) != tc.want {
					t.Fatalf("got %v, want %s", err, tc.want)
				}
			}
			_, after, err := store.Load()
			if err != nil || string(before) != string(after) {
				t.Fatalf("dry run saved configuration: %v", err)
			}
		})
	}
	old := followerCfg()
	old.Postgres.Settings = map[string]string{"max_worker_processes": "0xc8"}
	next := old
	next.Postgres.Settings = map[string]string{"max_worker_processes": "200.0"}
	if err := guardRecoveryFloorChanges(old, next, hostinfo.Info{VCPU: 4, RAMMiB: 8192}); err != nil {
		t.Fatalf("equivalent PostgreSQL representations: %v", err)
	}
}

func TestRecoveryFloorMemoryAndOverrides(t *testing.T) {
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192}
	tests := []struct {
		name   string
		before config.ClusterSpec
		after  config.ClusterSpec
		want   string
	}{
		{"named-size", config.ClusterSpec{Size: "medium"}, config.ClusterSpec{Size: "small"}, "has_followers"},
		{"custom-memory", config.ClusterSpec{Size: "custom", Memory: "1GiB"}, config.ClusterSpec{Size: "custom", Memory: "512MiB"}, "has_followers"},
		{"memory-override-named-size", config.ClusterSpec{Size: "medium", Memory: "1GiB"}, config.ClusterSpec{Size: "medium", Memory: "512MiB"}, ""},
		{"explicit-floor-masks-size", config.ClusterSpec{Size: "medium", Settings: map[string]string{"max_connections": "80"}}, config.ClusterSpec{Size: "small", Settings: map[string]string{"max_connections": "80"}}, ""},
		{"explicit-decrease", config.ClusterSpec{Size: "medium", Settings: map[string]string{"max_connections": "80"}}, config.ClusterSpec{Size: "medium", Settings: map[string]string{"max_connections": "60"}}, "has_followers"},
		{"custom-growth", config.ClusterSpec{Size: "custom", Memory: "512MiB"}, config.ClusterSpec{Size: "custom", Memory: "1GiB"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := followerCfg()
			tc.before.Name, tc.after.Name = "app", "app"
			old.Clusters[0] = tc.before
			next := old
			next.Clusters = append([]config.ClusterSpec(nil), old.Clusters...)
			next.Clusters[0] = tc.after
			err := guardRecoveryFloorChanges(old, next, host)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if tc.want != "" && (err == nil || reason(t, err) != tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestResizeConfirmationIsolationAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name              string
		tokens            []string
		deferRestart      bool
		source, follower  bool
		failFollowerFirst bool
	}{
		{"source-only", []string{"app/restart"}, false, true, false, false},
		{"follower-only", []string{"app_follower/restart"}, false, false, true, false},
		{"both", []string{"app/restart", "app_follower/restart"}, false, true, true, false},
		{"deferred-overrides-tokens", []string{"app/restart", "app_follower/restart"}, true, false, false, false},
		{"failed-follower-write-retry", []string{"app/restart", "app_follower/restart"}, false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			cfg.Clusters[0].Size = "small"
			env, _ := resizeFixture(t, cfg, "medium", false)
			env.Req.Confirms = tc.tokens
			if tc.deferRestart {
				env.Req = request("db.resize", map[string]any{"name": "app", "size": "medium", "defer_restart": true})
				env.Req.Confirms = tc.tokens
			}
			fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
				hostinfo.Memory{CommitLimitMiB: 16384, CommittedMiB: 1024, AvailableMiB: 8192, HugePageSizeKiB: 2048}, true)
			oldResize, oldFollower := applyResizeStep, applyFollowerStep
			t.Cleanup(func() { applyResizeStep, applyFollowerStep = oldResize, oldFollower })
			var calls []string
			fail := tc.failFollowerFirst
			applyResizeStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
				if opts.DB == "" {
					calls = append(calls, "kernel")
				} else {
					calls = append(calls, "source")
					if opts.Confirm.Covers("app") != tc.source || opts.Confirm.Covers("app_follower") != tc.follower {
						t.Errorf("source confirmation: %+v", opts.Confirm)
					}
				}
				return engine.ApplyData{}, nil, nil
			}
			applyFollowerStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
				calls = append(calls, "follower")
				if opts.DB != "app_follower" || !slices.Equal(opts.Only, []string{steps.ConfigStepID("app_follower")}) ||
					opts.Confirm.Covers("app") != tc.source || opts.Confirm.Covers("app_follower") != tc.follower {
					t.Errorf("follower options: %+v", opts)
				}
				if fail {
					fail = false
					return engine.ApplyData{}, nil, errors.New("follower configuration write failed")
				}
				return engine.ApplyData{}, nil, nil
			}
			_, err := handleDBResize(context.Background(), env)
			if tc.failFollowerFirst {
				if err == nil || !slices.Equal(calls, []string{"kernel", "follower"}) {
					t.Fatalf("first attempt: calls %v, err %v", calls, err)
				}
				calls = nil
				_, err = handleDBResize(context.Background(), env)
			}
			if err != nil || !slices.Equal(calls, []string{"kernel", "follower", "source"}) {
				t.Fatalf("final attempt: calls %v, err %v", calls, err)
			}
		})
	}
}

func TestConfigSetBatchFloorOverrides(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sourceAfter string
		followAfter string
		want        string
	}{
		{"masked-source", "100", "300", "has_followers"},
		{"follower-with-global-change", "200", "250", "follower_recovery_floor_decrease"},
	} {
		for _, full := range []bool{false, true} {
			for _, dry := range []bool{false, true} {
				t.Run(tc.name, func(t *testing.T) {
					cfg := followerLifecycleCfg(t, "app_follower")
					cfg.Postgres.Settings = map[string]string{"max_worker_processes": "150"}
					cfg.Clusters[0].Settings = map[string]string{"max_worker_processes": "200"}
					cfg.Clusters[1].Settings = map[string]string{"max_worker_processes": "300"}
					env, store := resizeFixture(t, cfg, "medium", dry)
					oldStore := configSetStore
					configSetStore = func() state.FileStore { return store }
					t.Cleanup(func() { configSetStore = oldStore })
					rev, err := store.Revision()
					if err != nil {
						t.Fatal(err)
					}
					_, before, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					params := map[string]any{"base_revision": rev}
					if full {
						cfg.Postgres.Settings = map[string]string{"max_worker_processes": "100"}
						cfg.Clusters[0].Settings = map[string]string{"max_worker_processes": tc.sourceAfter}
						cfg.Clusters[1].Settings = map[string]string{"max_worker_processes": tc.followAfter}
						raw, err := cfg.Encode()
						if err != nil {
							t.Fatal(err)
						}
						params["toml"] = string(raw)
					} else {
						params["set"] = map[string]string{
							"postgres.settings.max_worker_processes":              "100",
							"clusters.app.settings.max_worker_processes":          tc.sourceAfter,
							"clusters.app_follower.settings.max_worker_processes": tc.followAfter,
						}
					}
					env.Req = request("config.set", params)
					env.Req.DryRun = dry
					_, err = handleConfigSet(context.Background(), env)
					if err == nil || reason(t, err) != tc.want {
						t.Fatalf("full=%v dry=%v: %v, want %s", full, dry, err, tc.want)
					}
					_, after, err := store.Load()
					if err != nil || string(before) != string(after) {
						t.Fatalf("saved on refusal, err %v", err)
					}
				})
			}
		}
	}
}

func TestRecoveryFloorSourceRemovalBoundary(t *testing.T) {
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192}
	old := followerCfg()
	old.Clusters[0].Settings = map[string]string{"max_worker_processes": "200"}
	old.Clusters[1].Settings = map[string]string{"max_worker_processes": "300"}
	next := old
	next.Clusters = append([]config.ClusterSpec(nil), old.Clusters[:1]...)
	next.Clusters[0].Settings = map[string]string{"max_worker_processes": "100"}
	if err := guardRecoveryFloorChanges(old, next, host); err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("removing follower and lowering source together: %v", err)
	}
	next.Clusters[0] = old.Clusters[0]
	if err := guardRecoveryFloorChanges(old, next, host); err != nil {
		t.Fatalf("follower removal without source decrease: %v", err)
	}
}

func TestFullConfigRemovalDoesNotPermitSourceShrink(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[0].Size, cfg.Clusters[1].Size = "medium", "medium"
	env, store := resizeFixture(t, cfg, "small", false)
	oldStore := configSetStore
	configSetStore = func() state.FileStore { return store }
	t.Cleanup(func() { configSetStore = oldStore })
	rev, err := store.Revision()
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters[:1]...)
	cfg.Clusters[0].Size = "small"
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	env.Req = request("config.set", map[string]any{"base_revision": rev, "toml": string(raw)})
	_, err = handleConfigSet(context.Background(), env)
	if err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("removed follower masked source shrink: %v", err)
	}
	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("saved on refusal: %v", err)
	}
}

func TestGlobalDefaultSizeChangesSourceFloor(t *testing.T) {
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192}
	old := followerCfg()
	old.Postgres.DefaultSize = "medium"
	old.Clusters[0].Size = ""
	old.Clusters[1].Size = "large"
	next := old
	next.Postgres.DefaultSize = "small"
	err := guardRecoveryFloorChanges(old, next, host)
	if err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("global default shrink masked by follower: %v", err)
	}
	next.Postgres.DefaultSize = "large"
	if err := guardRecoveryFloorChanges(old, next, host); err != nil {
		t.Fatalf("global default growth: %v", err)
	}
}

func TestConfigSetGlobalDefaultSizeRefused(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			t.Run(fmt.Sprintf("full=%v/dry=%v", full, dry), func(t *testing.T) {
				cfg := followerLifecycleCfg(t, "app_follower")
				cfg.Postgres.DefaultSize = "medium"
				cfg.Clusters[0].Size, cfg.Clusters[1].Size = "", "large"
				env, store := resizeFixture(t, cfg, "", dry)
				oldStore := configSetStore
				configSetStore = func() state.FileStore { return store }
				t.Cleanup(func() { configSetStore = oldStore })
				revision, err := store.Revision()
				if err != nil {
					t.Fatal(err)
				}
				_, before, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				params := map[string]any{"base_revision": revision}
				if full {
					cfg.Postgres.DefaultSize = "small"
					raw, err := cfg.Encode()
					if err != nil {
						t.Fatal(err)
					}
					params["toml"] = string(raw)
				} else {
					params["set"] = map[string]string{"postgres.default_size": "small"}
				}
				env.Req = request("config.set", params)
				env.Req.DryRun = dry
				_, err = handleConfigSet(context.Background(), env)
				if err == nil || reason(t, err) != "has_followers" {
					t.Fatalf("got %v", err)
				}
				_, after, err := store.Load()
				if err != nil || string(before) != string(after) {
					t.Fatalf("saved on refusal: %v", err)
				}
			})
		}
	}
}

func TestResizeCustomMemoryDecreaseRefusesBeforeMutation(t *testing.T) {
	for _, follower := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			cfg := followerLifecycleCfg(t, "app_follower")
			cfg.Clusters[0].Size, cfg.Clusters[0].Memory = "custom", "1GiB"
			cfg.Clusters[1].Size, cfg.Clusters[1].Memory = "custom", "2GiB"
			name, want := "app", "has_followers"
			if follower {
				cfg.Clusters[0].Memory = "512MiB"
				cfg.Clusters[1].Memory = "1GiB"
				name, want = "app_follower", "follower_recovery_floor_decrease"
			}
			env, store := resizeFixture(t, cfg, "", dry)
			env.Req = request("db.resize", map[string]any{"name": name, "memory": "512MiB"})
			env.Req.DryRun = dry
			_, before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			oldResize, oldFollower := applyResizeStep, applyFollowerStep
			var calls int
			applyResizeStep = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
				calls++
				return engine.ApplyData{}, nil, nil
			}
			applyFollowerStep = applyResizeStep
			_, err = handleDBResize(context.Background(), env)
			applyResizeStep, applyFollowerStep = oldResize, oldFollower
			if err == nil || reason(t, err) != want {
				t.Fatalf("follower=%v dry=%v: %v", follower, dry, err)
			}
			_, after, err := store.Load()
			if err != nil || string(before) != string(after) || calls != 0 {
				t.Fatalf("follower=%v dry=%v: changed file or ran %d apply calls: %v", follower, dry, calls, err)
			}
		}
	}
}

// Adding a cluster changes CPU-share tuning of existing clusters. This is a
// real floor change, not an isolated resize; admission refuses it.
func TestAddingClusterChangesExistingRecoveryFloors(t *testing.T) {
	host := hostinfo.Info{VCPU: 8, RAMMiB: 8192}
	old := followerCfg()
	old.Clusters[0].Settings = nil
	next := old
	next.Clusters = append(append([]config.ClusterSpec(nil), old.Clusters...), config.ClusterSpec{Name: "other", Size: "small"})
	before, err := steps.OwnFloors(old, old.Clusters[0], host)
	if err != nil {
		t.Fatal(err)
	}
	after, err := steps.OwnFloors(next, next.Clusters[0], host)
	if err != nil {
		t.Fatal(err)
	}
	if before["max_worker_processes"] != 12 || after["max_worker_processes"] != 10 {
		t.Fatalf("source floors before=%v after=%v", before, after)
	}
	if err := guardRecoveryFloorChanges(old, next, host); err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("config-set guard did not observe changed source tuning: %v", err)
	}
}

func TestUnsafeAdditionRefusedAcrossCreateForkAndFollow(t *testing.T) {
	for _, op := range []string{"create", "fork", "follow"} {
		for _, dry := range []bool{false, true} {
			if op == "create" && dry {
				continue // The CLI's create preview is local, not an RPC.
			}
			t.Run(fmt.Sprintf("%s/dry=%v", op, dry), func(t *testing.T) {
				cfg := followerLifecycleCfg(t, "existing_follower")
				node := newFollowerNode(t, cfg)
				fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
					hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, true)
				oldExists, oldFollowExists := createClusterExists, followClusterExists
				createClusterExists = func(context.Context, string) bool { return false }
				followClusterExists = createClusterExists
				t.Cleanup(func() { createClusterExists, followClusterExists = oldExists, oldFollowExists })
				oldKernel, oldFollowJob, oldForkSpace, oldForkAcquire, oldForkJob := createApplyKernel, followStartJob, forkSpace, forkAcquire, forkStartJob
				defer func() {
					createApplyKernel, followStartJob, forkSpace, forkAcquire, forkStartJob = oldKernel, oldFollowJob, oldForkSpace, oldForkAcquire, oldForkJob
				}()
				calls := 0
				createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
					calls++
					return engine.ApplyData{}, nil, errors.New("unexpected apply")
				}
				followStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
					calls++
					return "", errors.New("unexpected follow job")
				}
				forkStartJob = followStartJob
				forkSpace = func(context.Context, config.Cluster, string) ([]backup.SpaceNeed, error) {
					calls++
					return nil, errors.New("unexpected fork space check")
				}
				forkAcquire = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
					calls++
					return func() {}, nil
				}
				_, before, err := node.store.Load()
				if err != nil {
					t.Fatal(err)
				}
				env := &Env{Cluster: cfg}
				switch op {
				case "create":
					env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
					env.Req.Secrets = copyRoleSecrets("newdb")
					_, err = handleDBCreate(context.Background(), env)
				case "fork":
					env.Req = request("db.fork", map[string]any{"source": "app", "name": "newdb", "size": "small"})
					env.Req.DryRun = dry
					_, err = handleDBFork(context.Background(), env)
				case "follow":
					env.Req = followRequest("app", "newdb", "small")
					env.Req.DryRun = dry
					_, err = handleDBFollow(context.Background(), env)
				}
				if err == nil || reason(t, err) != "has_followers" {
					t.Fatalf("got %v", err)
				}
				_, after, err := node.store.Load()
				if err != nil || string(before) != string(after) || calls != 0 {
					t.Fatalf("file changed or %d mutations/probes reached: %v", calls, err)
				}
			})
		}
	}
}

func TestUnsafeCopyRefusedBeforeTargetOrJob(t *testing.T) {
	for _, mode := range []string{"dry-run", "start", "preflight", "locked-job"} {
		t.Run(mode, func(t *testing.T) {
			n := newCopyTestNode(t)
			cfg := n.cfg
			cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "existing_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"})
			raw, err := cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if err := n.store.Save(raw); err != nil {
				t.Fatal(err)
			}
			detectHost = func(string) (hostinfo.Info, error) {
				return hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}, nil
			}
			_, before, err := n.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			copyClusterInventory = func(context.Context) (map[string]string, error) {
				calls++
				return nil, errors.New("unexpected target probe")
			}
			copyStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
				calls++
				return "", errors.New("unexpected job")
			}
			copyOpenWorkspace = func(string) (*backup.CopyWorkspace, error) {
				calls++
				return nil, errors.New("unexpected workspace")
			}
			p := dbCopyParams{Name: "copyapp", Size: "small"}
			env := &Env{Cluster: cfg, Req: request("db.copy", map[string]any{"name": p.Name, "size": p.Size}), Report: func(output.Event) {}}
			env.Req.Secrets = copyTestSecrets(p.Name)
			switch mode {
			case "dry-run":
				env.Req.DryRun = true
				_, err = handleDBCopy(context.Background(), env)
			case "start":
				_, err = handleDBCopy(context.Background(), env)
			case "preflight":
				_, err = handleDBCopyPreflight(context.Background(), env)
			case "locked-job":
				_, err = jobCopy(context.Background(), copyTestJobEnv(t, p, copyTestSecrets(p.Name)))
			}
			if err == nil || reason(t, err) != "has_followers" {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode == "locked-job" {
				details := copyErrorDetails(t, err)
				drops, _ := details["settings"].([]recoveryFloorDecrease)
				names, _ := details["followers"].([]string)
				if details["source_database"] != "app" || !slices.Contains(names, "existing_follower") ||
					!slices.Contains(drops, recoveryFloorDecrease{Setting: "max_worker_processes", Before: 12, After: 10}) {
					t.Fatalf("lost floor details: %+v", details)
				}
			}
			_, after, err := n.store.Load()
			if err != nil || string(before) != string(after) || calls != 0 {
				t.Fatalf("file changed or %d target/job/workspace callbacks: %v", calls, err)
			}
		})
	}
}

func TestFollowAdditionRechecksFreshTopologyUnderLock(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	node := newFollowerNode(t, cfg)
	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, true)
	installFollowAdmission(t, func(config.ClusterSpec) bool { return true })
	installPrepareTestLocks(t)
	oldExists, oldHook, oldJob := followClusterExists, followBeforePrepareLock, followStartJob
	t.Cleanup(func() { followClusterExists, followBeforePrepareLock, followStartJob = oldExists, oldHook, oldJob })
	followClusterExists = func(context.Context, string) bool { return false }
	started := false
	followStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		started = true
		return "", errors.New("unexpected job")
	}
	followBeforePrepareLock = func() {
		changed := node.config()
		changed.Clusters = append(changed.Clusters, config.ClusterSpec{
			Name: "existing_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app",
		})
		node.save(changed)
	}
	env := &Env{Cluster: cfg, Req: followRequest("app", "newdb", "small")}
	_, err := handleDBFollow(context.Background(), env)
	if err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("stale preview admitted: %v", err)
	}
	if _, ok := node.config().ClusterByName("newdb"); ok || started {
		t.Fatal("unsafe locked proposal was saved or started")
	}
}

func TestCopyJobReportsAffectedFollowerFloor(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.cfg
	cfg.Clusters[0].Settings = map[string]string{"max_worker_processes": "10"}
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
		Name: "existing_follower", Node: "db1", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app",
	})
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	detectHost = func(string) (hostinfo.Info, error) {
		return hostinfo.Info{VCPU: 8, RAMMiB: 8192}, nil
	}
	_, err = jobCopy(context.Background(), copyTestJobEnv(t, dbCopyParams{Name: "copyapp", Size: "small"}, copyTestSecrets("copyapp")))
	if err == nil || reason(t, err) != "follower_recovery_floor_decrease" {
		t.Fatalf("got %v", err)
	}
	details := copyErrorDetails(t, err)
	drops, _ := details["settings"].([]recoveryFloorDecrease)
	if details["database"] != "copyapp" || details["follower_database"] != "existing_follower" || details["source"] != "app" ||
		!slices.Contains(drops, recoveryFloorDecrease{Setting: "max_worker_processes", Before: 12, After: 10}) {
		t.Fatalf("lost follower floor facts: %+v", details)
	}
	if _, ok := n.saved().ClusterByName("copyapp"); ok {
		t.Fatal("copy target was saved")
	}
}

func TestSafeAdditionStillSaves(t *testing.T) {
	for _, tc := range []struct {
		name string
		vcpu int
		pin  bool
	}{
		{"unchanged-cpu-share", 4, false},
		{"explicit-settings-mask-cpu-share", 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "existing_follower")
			if tc.pin {
				cfg.Clusters[0].Settings = map[string]string{"max_worker_processes": "12"}
				cfg.Clusters[1].Settings = map[string]string{"max_worker_processes": "12"}
			}
			env, store := resizeFixture(t, cfg, "", false)
			fixedResizeHost(t, hostinfo.Info{VCPU: tc.vcpu, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
				hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
			installDeterministicAddChecks(t)
			oldExists := createClusterExists
			createClusterExists = func(context.Context, string) bool { return false }
			t.Cleanup(func() { createClusterExists = oldExists })
			env.Req.Secrets = copyRoleSecrets("newdb")
			calls := 0
			_, _, _, err := addDatabaseFrom(context.Background(), env, func(config.Cluster) (config.ClusterSpec, error) {
				return config.ClusterSpec{Name: "newdb", Size: "small", StatementTimeout: "60s"}, nil
			}, false, nil, func(config.Cluster, config.ClusterSpec) error {
				calls++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			saved, _, err := store.Load()
			if err != nil || calls != 1 {
				t.Fatalf("save/callback failed: %d, %v", calls, err)
			}
			if _, ok := saved.ClusterByName("newdb"); !ok {
				t.Fatal("safe addition was not saved")
			}
		})
	}
}

func TestFullConfigAdditionRefusesChangedCPUFloor(t *testing.T) {
	for _, dry := range []bool{false, true} {
		cfg := followerLifecycleCfg(t, "existing_follower")
		env, store := resizeFixture(t, cfg, "", dry)
		fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192},
			hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
		oldStore := configSetStore
		configSetStore = func() state.FileStore { return store }
		t.Cleanup(func() { configSetStore = oldStore })
		revision, err := store.Revision()
		if err != nil {
			t.Fatal(err)
		}
		_, before, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
			Name: "newdb", Node: "db1", Port: 5435, Stanza: "newdb-3f9a1c", Size: "small", StatementTimeout: "60s",
		})
		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		env.Req = request("config.set", map[string]any{"base_revision": revision, "toml": string(raw)})
		env.Req.DryRun = dry
		_, err = handleConfigSet(context.Background(), env)
		if err == nil || reason(t, err) != "has_followers" {
			t.Fatalf("dry=%v: %v", dry, err)
		}
		_, after, err := store.Load()
		if err != nil || string(before) != string(after) {
			t.Fatalf("saved on refusal: %v", err)
		}
	}
}

func TestAdditionFollowerDecreaseAndGrowingBatch(t *testing.T) {
	host := hostinfo.Info{VCPU: 8, RAMMiB: 8192}
	old := followerCfg()
	old.Clusters[0].Settings = map[string]string{"max_worker_processes": "10"}
	added := config.ClusterSpec{Name: "newdb", Size: "small"}
	if err := guardRecoveryFloorAddition(old, added, host); err == nil || reason(t, err) != "follower_recovery_floor_decrease" {
		t.Fatalf("existing follower floor decrease: %v", err)
	}
	next := old
	next.Clusters = append(append([]config.ClusterSpec(nil), old.Clusters...), added)
	next.Clusters[0].Settings = map[string]string{"max_worker_processes": "13"}
	if err := guardRecoveryFloorChanges(old, next, host); err != nil {
		t.Fatalf("batch addition with increasing effective floors: %v", err)
	}
}

func TestDBCreateDryRunFloorAdmissionAndNoProvision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vcpu   int
		reason string
	}{
		{"unsafe", 8, "has_followers"},
		{"safe", 4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "existing_follower")
			env, store := resizeFixture(t, cfg, "", true)
			env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
			env.Req.DryRun = true
			fixedResizeHost(t, hostinfo.Info{VCPU: tc.vcpu, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
				hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
			starts := 0
			oldExists, oldStarts, oldKernel := createClusterExists, addDatabaseCheckStarts, createApplyKernel
			t.Cleanup(func() {
				createClusterExists, addDatabaseCheckStarts, createApplyKernel = oldExists, oldStarts, oldKernel
			})
			createClusterExists = func(context.Context, string) bool { return false }
			addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error {
				starts++
				return nil
			}
			applies := 0
			createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
				applies++
				return engine.ApplyData{}, nil, errors.New("unexpected kernel apply")
			}
			_, before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			res, err := handleDBCreate(context.Background(), env)
			if tc.reason != "" {
				if err == nil || reason(t, err) != tc.reason || starts != 0 {
					t.Fatalf("unsafe preview: err=%v starts=%d", err, starts)
				}
			} else {
				if err != nil || starts != 1 {
					t.Fatalf("safe preview: err=%v starts=%d", err, starts)
				}
				data, _ := res.Data.(map[string]any)
				db, ok := data["database"].(config.ClusterSpec)
				if !ok || db.Name != "newdb" || db.Size != "small" || db.Node != "db1" || db.Port != 5435 || db.Stanza == "" {
					t.Fatalf("planned database: %+v", data["database"])
				}
			}
			_, after, err := store.Load()
			if err != nil || string(before) != string(after) || applies != 0 {
				t.Fatalf("dry run saved or applied: err=%v applies=%d", err, applies)
			}
			if _, ok := env.Cluster.ClusterByName("newdb"); ok {
				t.Fatal("dry run mutated the input cluster snapshot")
			}
		})
	}
}

func TestDBCreateDryRunRetainsAdmissionChecks(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", true)
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
	env.Req.DryRun = true
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 2048, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
	oldExists, oldStarts, oldProtect := createClusterExists, addDatabaseCheckStarts, defaultProtectGuard
	t.Cleanup(func() {
		createClusterExists, addDatabaseCheckStarts, defaultProtectGuard = oldExists, oldStarts, oldProtect
	})
	createClusterExists = func(context.Context, string) bool { return false }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "memory_overcommitted" {
		t.Fatalf("missing budget refusal: %v", err)
	}
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small", "allow_overcommit": true})
	env.Req.DryRun = true
	res, err := handleDBCreate(context.Background(), env)
	if err != nil {
		t.Fatalf("overcommit preview: %v", err)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || w.Code == "memory_overcommitted"
	}
	if !found {
		t.Fatalf("missing overcommit warning: %+v", res.Warnings)
	}
	env.Req = request("db.create", map[string]any{"name": "app", "size": "small"})
	env.Req.DryRun = true
	if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "database_exists" {
		t.Fatalf("preview bypassed existing database check: %v", err)
	}
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "not-a-size"})
	env.Req.DryRun = true
	if _, err := handleDBCreate(context.Background(), env); err == nil {
		t.Fatal("preview bypassed size validation")
	}
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error {
		return output.NewError(output.CodePrecondition, "cannot start", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "would_not_start"})
	}
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
	env.Req.DryRun = true
	if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "would_not_start" {
		t.Fatalf("preview bypassed start check: %v", err)
	}
	defaultProtectGuard = protectGuard{path: filepath.Join(t.TempDir(), "missing"), now: time.Now}
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small", "protected": true})
	env.Req.DryRun = true
	if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "protect_not_initialized" {
		t.Fatalf("preview bypassed protection check: %v", err)
	}
	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("preview saved configuration: %v", err)
	}
}

func TestAddDatabaseDryRunSkipsBeforeSaveCallback(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", true)
	env.Req.DryRun = true
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
	oldExists, oldStarts := createClusterExists, addDatabaseCheckStarts
	t.Cleanup(func() { createClusterExists, addDatabaseCheckStarts = oldExists, oldStarts })
	createClusterExists = func(context.Context, string) bool { return false }
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	called := false
	proposed, _, db, err := addDatabaseFrom(context.Background(), env, func(config.Cluster) (config.ClusterSpec, error) {
		return config.ClusterSpec{Name: "newdb", Size: "small", StatementTimeout: "60s"}, nil
	}, false, nil, func(config.Cluster, config.ClusterSpec) error {
		called = true
		return errors.New("unexpected before-save callback")
	})
	if err != nil || called || db.Name != "newdb" {
		t.Fatalf("preview: db=%+v callback=%v err=%v", db, called, err)
	}
	if _, ok := proposed.ClusterByName("newdb"); !ok {
		t.Fatal("proposed topology lacks newdb")
	}
	saved, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.ClusterByName("newdb"); ok {
		t.Fatal("preview saved newdb")
	}
}

func TestDBCreateDryRunReadsFreshTopology(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", true)
	updated := followerLifecycleCfg(t, "existing_follower")
	updated.Server.DataRoot = cfg.Server.DataRoot
	raw, err := updated.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(raw); err != nil {
		t.Fatal(err)
	}
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, false)
	oldExists := createClusterExists
	createClusterExists = func(context.Context, string) bool { return false }
	t.Cleanup(func() { createClusterExists = oldExists })
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
	env.Req.DryRun = true
	if _, err := handleDBCreate(context.Background(), env); err == nil || reason(t, err) != "has_followers" {
		t.Fatalf("stale env masked fresh follower: %v", err)
	}
	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("dry run saved configuration: %v", err)
	}
}

func TestDBCreateRealRunStillRequiresRoleSecrets(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", false)
	env.Req = request("db.create", map[string]any{"name": "newdb", "size": "small"})
	oldExists := createClusterExists
	createClusterExists = func(context.Context, string) bool { return false }
	t.Cleanup(func() { createClusterExists = oldExists })
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, err = handleDBCreate(context.Background(), env)
	var cli *output.CLIError
	if !errors.As(err, &cli) || cli.Code != output.CodeInputRequired {
		t.Fatalf("real create accepted missing role secrets: %v", err)
	}
	_, after, err := store.Load()
	if err != nil || string(before) != string(after) {
		t.Fatalf("real create saved without role secrets: %v", err)
	}
}
