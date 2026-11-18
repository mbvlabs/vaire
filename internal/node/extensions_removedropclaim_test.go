package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func removeDropCaseName(installed bool) string {
	if installed {
		return "drop executed"
	}

	return "sql already absent"
}

func removeDropInstalled(installed bool) func(int) (map[string]extensionInstalledVersion, error) {
	return func(int) (map[string]extensionInstalledVersion, error) {
		if installed {
			return installedVersion("1.6", "1.6"), nil
		}

		return map[string]extensionInstalledVersion{}, nil
	}
}

func removeDropWantExecs(installed bool) int {
	if installed {
		return 1
	}

	return 0
}

func removeDropAssertMessageAvoidsFalseClaim(t *testing.T, err error, installed bool) {
	t.Helper()

	cliErr := removeHistoryCLIError(t, err)
	if !installed && (strings.Contains(cliErr.Message, "SQL is dropped") ||
		strings.Contains(cliErr.Message, "SQL drop ran")) {
		t.Fatalf("no-DROP failure claimed a DROP: %q", cliErr.Message)
	}
}

func TestExtRemoveConfigFailureReportsActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
			h.installedFn = removeDropInstalled(installed)
			h.pg.queryFn = removeHistoryDependentsZero
			h.applyFn = func(engine.Options) (engine.ApplyData, error) {
				return engine.ApplyData{}, errors.New("config write failed")
			}

			env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_apply_failed" {
				t.Fatalf("config failure = %v", err)
			}

			details := extDetails(t, err)
			if details["sql_dropped"] != installed || details["intent_saved"] != true {
				t.Fatalf("config failure details = %+v", details)
			}

			if len(h.pg.execs) != removeDropWantExecs(installed) || len(h.saves) != 1 || len(h.applyCalls) != 1 {
				t.Fatalf("config failure effects: exec=%v saves=%d apply=%d",
					h.pg.execs, len(h.saves), len(h.applyCalls))
			}

			removeDropAssertMessageAvoidsFalseClaim(t, err, installed)
		})
	}
}

func TestExtRemovePendingRestartReportsActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
			h.installedFn = removeDropInstalled(installed)
			h.pg.queryFn = removeHistoryDependentsZero
			h.applyFn = func(engine.Options) (engine.ApplyData, error) {
				return engine.ApplyData{RestartPending: []string{steps.ConfigStepID("app")}}, nil
			}

			env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_pending_restart" {
				t.Fatalf("pending restart = %v", err)
			}

			details := extDetails(t, err)
			if details["sql_dropped"] != installed || details["intent_saved"] != true {
				t.Fatalf("pending restart details = %+v", details)
			}

			if len(h.pg.execs) != removeDropWantExecs(installed) {
				t.Fatalf("pending restart effects: exec=%v", h.pg.execs)
			}

			removeDropAssertMessageAvoidsFalseClaim(t, err, installed)
		})
	}
}

func TestExtRemoveRestartFailureReportsActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			cfg := extTestCfg("pg_stat_statements", "pg_trgm")
			cfg.Extensions = map[string]config.ExtensionDefinition{
				"pg_trgm": {Settings: map[string]string{"max_files_per_process": "2000"}},
			}
			h := newExtHarness(t, cfg)
			h.installedFn = removeDropInstalled(installed)
			h.pg.queryFn = removeHistoryDependentsZero
			h.pendingSettings = []string{"max_files_per_process"}
			h.restartErr = errors.New("restart failed")

			env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app", "app/restart")}

			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_apply_failed" {
				t.Fatalf("restart failure = %v", err)
			}

			details := extDetails(t, err)
			if details["sql_dropped"] != installed || details["intent_saved"] != true || details["restarted"] != false {
				t.Fatalf("restart failure details = %+v", details)
			}

			if len(h.pg.execs) != removeDropWantExecs(installed) || h.restartCalls != 1 {
				t.Fatalf("restart failure effects: exec=%v restarts=%d", h.pg.execs, h.restartCalls)
			}

			removeDropAssertMessageAvoidsFalseClaim(t, err, installed)
		})
	}
}

func TestExtRemoveVerifyFailureReportsActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
			h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
				if call == 1 {
					return removeDropInstalled(installed)(call)
				}

				return nil, errors.New("catalog unavailable")
			}
			h.pg.queryFn = removeHistoryDependentsZero

			env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_verify_failed" {
				t.Fatalf("verify failure = %v", err)
			}

			details := extDetails(t, err)
			if details["sql_dropped"] != installed || details["intent_saved"] != true {
				t.Fatalf("verify failure details = %+v", details)
			}

			cliErr := removeHistoryCLIError(t, err)
			if installed != strings.Contains(cliErr.Message, "SQL drop ran") {
				t.Fatalf("verify failure message = %q", cliErr.Message)
			}

			if len(h.pg.execs) != removeDropWantExecs(installed) {
				t.Fatalf("verify failure effects: exec=%v", h.pg.execs)
			}
		})
	}
}

func TestExtRemoveStillInstalledReportsActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
			h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
				if call == 1 {
					return removeDropInstalled(installed)(call)
				}

				return installedVersion("1.6", "1.6"), nil
			}
			h.pg.queryFn = removeHistoryDependentsZero

			env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_drop_failed" {
				t.Fatalf("still-installed verification = %v", err)
			}

			details := extDetails(t, err)
			if details["sql_dropped"] != installed || details["intent_saved"] != true {
				t.Fatalf("still-installed details = %+v", details)
			}

			cliErr := removeHistoryCLIError(t, err)
			if installed != strings.Contains(cliErr.Message, "after DROP") {
				t.Fatalf("still-installed message = %q", cliErr.Message)
			}

			if len(h.pg.execs) != removeDropWantExecs(installed) {
				t.Fatalf("still-installed effects: exec=%v", h.pg.execs)
			}
		})
	}
}

func TestExtRemoveSQLOnlyVerifyFailureReportsExecutedDrop(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return installedVersion("1.6", "1.6"), nil
		}

		return nil, errors.New("catalog unavailable")
	}
	h.pg.queryFn = removeHistoryDependentsZero

	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_verify_failed" {
		t.Fatalf("SQL-only verify failure = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != true || details["intent_saved"] != false {
		t.Fatalf("SQL-only verify details = %+v", details)
	}

	if len(h.pg.execs) != 1 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("SQL-only verify effects: exec=%v", h.pg.execs)
	}
}

func newRemoveRetryHarness(t *testing.T) *extHarness {
	t.Helper()
	h := newExtHarness(t, extTestCfg("pg_stat_statements"))
	h.pendingSettings = []string{"max_files_per_process"}

	return h
}

func testRemoveRetryConfigFailure(t *testing.T) {
	params := map[string]any{"name": "pg_trgm", "db": "app"}
	h := newRemoveRetryHarness(t)
	h.applyFn = func(engine.Options) (engine.ApplyData, error) {
		return engine.ApplyData{}, errors.New("config write failed")
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, params, "app", "app/restart")}

	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_apply_failed" {
		t.Fatalf("retry config failure = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != false || details["intent_saved"] != true {
		t.Fatalf("retry config details = %+v", details)
	}

	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 1 {
		t.Fatalf("retry config effects: exec=%v saves=%d apply=%d",
			h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}

func testRemoveRetryVerificationFailure(t *testing.T) {
	params := map[string]any{"name": "pg_trgm", "db": "app"}
	h := newRemoveRetryHarness(t)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}

		return nil, errors.New("catalog unavailable")
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, params, "app", "app/restart")}

	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_verify_failed" {
		t.Fatalf("retry verification failure = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != false || details["intent_saved"] != true {
		t.Fatalf("retry verification details = %+v", details)
	}

	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 1 {
		t.Fatalf("retry verification effects: exec=%v saves=%d apply=%d",
			h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}

func TestExtRemoveRetryConfigFailureReportsNoSQLDrop(t *testing.T) {
	t.Run("config apply failure", testRemoveRetryConfigFailure)
	t.Run("verification failure", testRemoveRetryVerificationFailure)
}

func newFollowerRemoveFailureHarness(t *testing.T, installed bool) (*extHarness, config.Cluster) {
	t.Helper()

	cfg := extFollowerCfg(t)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"pg_stat_statements", "pg_trgm"}
	}

	h := newExtHarness(t, cfg)
	h.installedFn = removeDropInstalled(installed)
	h.pg.queryFn = removeHistoryDependentsZero

	return h, cfg
}

func followerRemoveEnv(t *testing.T, cfg config.Cluster) *Env {
	return &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"},
		"app", "app/restart", "app_follower_1/restart", "app_follower_2/restart")}
}

func assertFollowerRemoveFailure(t *testing.T, h *extHarness, err error, installed bool, wantReason string, wantCompleted, wantPending []string) {
	t.Helper()

	if err == nil || extReason(t, err) != wantReason {
		t.Fatalf("follower failure = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != installed || details["intent_saved"] != true {
		t.Fatalf("follower failure details = %+v", details)
	}

	if completed, _ := details["completed"].([]string); !slices.Equal(completed, wantCompleted) {
		t.Fatalf("follower failure completed = %#v", details["completed"])
	}

	if pending, _ := details["pending"].([]string); !slices.Equal(pending, wantPending) {
		t.Fatalf("follower failure pending = %#v", details["pending"])
	}

	if len(h.pg.execs) != removeDropWantExecs(installed) {
		t.Fatalf("follower failure effects: exec=%v", h.pg.execs)
	}

	removeDropAssertMessageAvoidsFalseClaim(t, err, installed)
}

func testFollowerRemovePrimaryConfigFailure(t *testing.T, installed bool) {
	t.Helper()
	h, cfg := newFollowerRemoveFailureHarness(t, installed)
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app" {
			return engine.ApplyData{}, errors.New("primary config failed")
		}

		return engine.ApplyData{}, nil
	}
	_, err := handleExtRemove(context.Background(), followerRemoveEnv(t, cfg))
	assertFollowerRemoveFailure(t, h, err, installed, "extension_apply_failed", nil, []string{"app"})
}

func testFollowerRemoveFollowerConfigFailure(t *testing.T, installed bool) {
	t.Helper()
	h, cfg := newFollowerRemoveFailureHarness(t, installed)
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB != "app" {
			return engine.ApplyData{}, errors.New("follower config failed")
		}

		return engine.ApplyData{}, nil
	}
	_, err := handleExtRemove(context.Background(), followerRemoveEnv(t, cfg))
	assertFollowerRemoveFailure(t, h, err, installed, "extension_apply_failed", []string{"app"}, []string{"app_follower_1"})
}

func testFollowerRemoveFollowerPreloadMissing(t *testing.T, installed bool) {
	t.Helper()
	h, cfg := newFollowerRemoveFailureHarness(t, installed)
	h.preloadLoaded = map[string]bool{}
	h.preloadPending = false
	_, err := handleExtRemove(context.Background(), followerRemoveEnv(t, cfg))
	assertFollowerRemoveFailure(t, h, err, installed, "extension_preload_missing", []string{"app"}, []string{"app_follower_1"})
}

func TestExtRemoveFollowerFailureBoundariesReportActualSQLDrop(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(removeDropCaseName(installed), func(t *testing.T) {
			t.Run("primary config failure", func(t *testing.T) {
				testFollowerRemovePrimaryConfigFailure(t, installed)
			})
			t.Run("follower config failure", func(t *testing.T) {
				testFollowerRemoveFollowerConfigFailure(t, installed)
			})
			t.Run("follower preload missing", func(t *testing.T) {
				testFollowerRemoveFollowerPreloadMissing(t, installed)
			})
		})
	}
}

func TestExtRemoveFollowerReplayPendingWithoutDropReportsNoSQLDrop(t *testing.T) {
	cfg := extFollowerCfg(t)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"pg_cron"}
	}

	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = removeDropInstalled(false)
	restoreSQL := steps.SetFollowerInstalledExtensionsForTest(func(_ context.Context, spec config.ClusterSpec) (map[string]bool, error) {
		if spec.Name == "app_follower_1" {
			return map[string]bool{"plpgsql": true, "pg_cron": true}, nil
		}

		return map[string]bool{"plpgsql": true}, nil
	})
	t.Cleanup(restoreSQL)

	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"},
		"app", "app/restart", "app_follower_1/restart", "app_follower_2/restart")}

	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_follower_replay_pending" {
		t.Fatalf("follower replay pending = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != false || details["intent_saved"] != true {
		t.Fatalf("follower replay details = %+v", details)
	}

	wantCompleted := []string{"app", "app_follower_1 (staged, pending)", "app_follower_2"}
	if completed, _ := details["completed"].([]string); !slices.Equal(completed, wantCompleted) {
		t.Fatalf("follower replay completed = %#v", details["completed"])
	}

	if pending, _ := details["pending"].([]string); !slices.Equal(pending, []string{"app_follower_1"}) {
		t.Fatalf("follower replay pending = %#v", details["pending"])
	}

	if len(h.pg.execs) != 0 {
		t.Fatalf("a no-DROP follower pending ran SQL: %v", h.pg.execs)
	}

	removeDropAssertMessageAvoidsFalseClaim(t, err, false)
}
