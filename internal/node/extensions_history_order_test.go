package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

func extensionRequirementNames(t *testing.T) []string {
	t.Helper()

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	retained := recs.RetainedExtensions("app-abc123")

	names := make([]string, 0, len(retained))
	for _, ext := range retained {
		names = append(names, ext.Name)
	}

	return names
}

func containsExtension(names []string, want string) bool {
	return slices.Contains(names, want)
}

func TestExtAddWithFollowersLastPrepareFailureLeavesNoIncomingHistory(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldPrepare := extensionPrepareStart
	extensionPrepareStart = func(_ context.Context, _ config.Cluster, spec config.ClusterSpec, _ []steps.ExtensionPrerequisite, _ engine.Runtime, _ engine.Reporter) error {
		h.prepareCalls++

		if spec.Name == "app_follower_2" {
			return errors.New("package unavailable")
		}

		return nil
	}

	t.Cleanup(func() { extensionPrepareStart = oldPrepare })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "package_install_failed" {
		t.Fatalf("last-target package failure = %v", err)
	}

	if h.prepareCalls != 3 {
		t.Fatalf("prepared %d targets, want all three before incoming history", h.prepareCalls)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("failed package preparation had later effects: saves=%d apply=%d exec=%d", len(h.saves), len(h.applyCalls), len(h.pg.execs))
	}

	if names := extensionRequirementNames(t); containsExtension(names, "pg_cron") {
		t.Fatalf("failed preparation recorded the speculative incoming provider: %v", names)
	}

	details := extDetails(t, err)
	if saved, _ := details["intent_saved"].(bool); saved {
		t.Fatalf("package failure claimed a saved intent: %v", details)
	}

	if mayRemain, _ := details["packages_may_remain"].(bool); !mayRemain {
		t.Fatalf("package failure hid possible package effects: %v", details)
	}

	if failed, _ := details["failed_target"].(string); failed != "app_follower_2" {
		t.Fatalf("failed target = %q, details %v", failed, details)
	}
}

func TestExtAddWithFollowersOutgoingCaptureFailureHasNoPackageEffect(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldOutgoing, oldIncoming := extensionCaptureOutgoing, extensionCaptureIncoming
	extensionCaptureOutgoing = func(config.Cluster) error { return errors.New("history unavailable") }
	incomingCalls := 0
	extensionCaptureIncoming = func(config.Cluster) error {
		incomingCalls++
		return nil
	}

	t.Cleanup(func() { extensionCaptureOutgoing, extensionCaptureIncoming = oldOutgoing, oldIncoming })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("outgoing capture failure = %v", err)
	}

	if h.prepareCalls != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || incomingCalls != 0 || h.restartCalls != 0 {
		t.Fatalf("outgoing capture failure had effects: prepare=%d saves=%d apply=%d exec=%d incoming=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), incomingCalls, h.restartCalls)
	}

	if strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("outgoing capture failure falsely claimed nothing changed: %v", err)
	}
}

func TestExtAddWithFollowersIncomingCaptureFailureAfterPreparation(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	outgoingCalls := 0
	oldOutgoing, oldIncoming := extensionCaptureOutgoing, extensionCaptureIncoming
	extensionCaptureOutgoing = func(config.Cluster) error {
		outgoingCalls++
		return nil
	}
	extensionCaptureIncoming = func(config.Cluster) error { return errors.New("history unavailable") }

	t.Cleanup(func() { extensionCaptureOutgoing, extensionCaptureIncoming = oldOutgoing, oldIncoming })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("incoming capture failure = %v", err)
	}

	if outgoingCalls != 1 {
		t.Fatalf("outgoing capture calls = %d, want one", outgoingCalls)
	}

	if h.prepareCalls != 3 {
		t.Fatalf("incoming capture ran before every target was prepared: %d", h.prepareCalls)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("incoming capture failure had later effects: saves=%d apply=%d exec=%d restart=%d",
			len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}

	details := extDetails(t, err)
	if saved, _ := details["intent_saved"].(bool); saved {
		t.Fatalf("incoming capture failure claimed a saved intent: %v", details)
	}

	if mayRemain, _ := details["packages_may_remain"].(bool); !mayRemain {
		t.Fatalf("incoming capture failure hid package effects: %v", details)
	}

	if historyMayRemain, _ := details["incoming_history_may_remain"].(bool); !historyMayRemain {
		t.Fatalf("incoming capture failure hid possible partial history: %v", details)
	}

	if strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("incoming capture failure falsely claimed nothing changed: %v", err)
	}
}

func TestExtAddWithFollowersLaterFailureRetainsIncomingHistory(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app_follower_1" {
			return engine.ApplyData{}, errors.New("follower apply failed")
		}

		return engine.ApplyData{}, nil
	}

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_apply_failed" {
		t.Fatalf("later follower failure = %v", err)
	}

	if len(h.saves) != 1 {
		t.Fatalf("the incoming intent was not saved before followers: %d", len(h.saves))
	}

	if names := extensionRequirementNames(t); !containsExtension(names, "pg_cron") {
		t.Fatalf("a later failure erased the incoming requirement history: %v", names)
	}
}

func TestExtAddWithFollowersOutgoingPartialWriteIsRetained(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldOutgoing, oldIncoming := extensionCaptureOutgoing, extensionCaptureIncoming
	extensionCaptureOutgoing = func(cfg config.Cluster) error {
		if err := oldOutgoing(cfg); err != nil {
			return err
		}

		return errors.New("history unavailable after outgoing write")
	}
	incomingCalls := 0
	extensionCaptureIncoming = func(config.Cluster) error {
		incomingCalls++
		return nil
	}

	t.Cleanup(func() { extensionCaptureOutgoing, extensionCaptureIncoming = oldOutgoing, oldIncoming })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("partial outgoing write failure = %v", err)
	}

	names := extensionRequirementNames(t)
	if !containsExtension(names, "pg_stat_statements") || !containsExtension(names, "auto_explain") {
		t.Fatalf("valid outgoing history was not retained: %v", names)
	}

	if containsExtension(names, "pg_cron") {
		t.Fatalf("a failed outgoing capture retained speculative incoming history: %v", names)
	}

	if h.prepareCalls != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || incomingCalls != 0 || h.restartCalls != 0 {
		t.Fatalf("partial outgoing write had later effects: prepare=%d saves=%d apply=%d exec=%d incoming=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), incomingCalls, h.restartCalls)
	}

	if strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("partial outgoing write falsely claimed nothing changed: %v", err)
	}
}

func TestExtAddWithFollowersAlreadyEnabledIncomingFailureKeepsIntentSaved(t *testing.T) {
	cfg := extFollowerCfg(t)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"pg_cron"}
	}

	h := newExtHarness(t, cfg)
	h.journalConverged = false
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	oldIncoming := extensionCaptureIncoming
	extensionCaptureIncoming = func(config.Cluster) error { return errors.New("history unavailable") }

	t.Cleanup(func() { extensionCaptureIncoming = oldIncoming })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("already-enabled incoming failure = %v", err)
	}

	details := extDetails(t, err)
	if saved, _ := details["intent_saved"].(bool); !saved {
		t.Fatalf("already-enabled unchanged intent was reported unsaved: %v", details)
	}

	if mayRemain, _ := details["packages_may_remain"].(bool); !mayRemain {
		t.Fatalf("already-enabled incoming failure hid package effects: %v", details)
	}

	if mayRemain, _ := details["incoming_history_may_remain"].(bool); !mayRemain {
		t.Fatalf("already-enabled incoming failure hid partial history: %v", details)
	}

	if h.prepareCalls != 3 || len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("already-enabled incoming failure effects: prepare=%d saves=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestExtAddWithFollowersIncomingPartialWriteIsDisclosed(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldIncoming := extensionCaptureIncoming
	extensionCaptureIncoming = func(incoming config.Cluster) error {
		if err := oldIncoming(incoming); err != nil {
			return err
		}

		return errors.New("history unavailable after incoming write")
	}

	t.Cleanup(func() { extensionCaptureIncoming = oldIncoming })

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("partial incoming write failure = %v", err)
	}

	details := extDetails(t, err)
	if mayRemain, _ := details["incoming_history_may_remain"].(bool); !mayRemain {
		t.Fatalf("partial incoming history was not disclosed: %v", details)
	}

	if strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("partial incoming history falsely claimed nothing changed: %v", err)
	}

	if names := extensionRequirementNames(t); !containsExtension(names, "pg_cron") {
		t.Fatalf("the incoming write was not actually durable: %v", names)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("partial incoming history had later effects: saves=%d apply=%d exec=%d restart=%d",
			len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestExtAddWithFollowersSaveFailureAfterPreparationIsTypedPartial(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.saveErrAt[1] = errors.New("disk full")

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("save failure after preparation = %v", err)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != false || details["packages_may_remain"] != true ||
		details["incoming_history_retained"] != true || details["config_rollback"] != "not_needed" {
		t.Fatalf("save failure details = %v", details)
	}

	if len(h.saveAttempts) != 1 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("save failure effects: attempts=%d apply=%d exec=%d restart=%d",
			len(h.saveAttempts), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}

	if names := extensionRequirementNames(t); !containsExtension(names, "pg_cron") {
		t.Fatalf("the incoming history was not retained after save failure: %v", names)
	}
}

func pgCronRemoveRequest() rpc.Request {
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}

	return req
}

func TestExtRemoveWithFollowersCapturePartialRetainsHistory(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
	}
	oldCapture := extensionCaptureRequirements
	extensionCaptureRequirements = func(ctx context.Context, oldCfg config.Cluster, oldSpec config.ClusterSpec, newCfg config.Cluster, newSpec config.ClusterSpec, action string) error {
		if err := oldCapture(ctx, oldCfg, oldSpec, newCfg, newSpec, action); err != nil {
			return err
		}

		return errors.New("history unavailable after capture")
	}

	t.Cleanup(func() { extensionCaptureRequirements = oldCapture })

	_, err := extensionRemoveExecute(context.Background(), &Env{Req: pgCronRemoveRequest(), Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("partial remove capture = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != false || details["intent_saved"] != false ||
		details["reduced_intent_saved"] != false || details["configuration_applied"] != false || details["restarted"] != false {
		t.Fatalf("partial remove capture details = %v", details)
	}

	if details["outgoing_history_may_remain"] != true || details["incoming_history_may_remain"] != true {
		t.Fatalf("partial remove capture hid retained history: %v", details)
	}

	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("partial remove capture crossed DROP/save/config: exec=%d saves=%d apply=%d",
			len(h.pg.execs), len(h.saves), len(h.applyCalls))
	}

	if names := extensionRequirementNames(t); !containsExtension(names, "pg_cron") {
		t.Fatalf("captured history was not durable before the failure: %v", names)
	}
}

func TestExtRemoveWithFollowersSaveFailureAfterDropIsTypedPartial(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
	}
	h.saveErrAt[1] = errors.New("disk full")

	_, err := extensionRemoveExecute(context.Background(), &Env{Req: pgCronRemoveRequest(), Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("post-DROP save failure = %v", err)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != true || details["intent_saved"] != false ||
		details["reduced_intent_saved"] != false || details["configuration_applied"] != false || details["restarted"] != false {
		t.Fatalf("post-DROP save details = %v", details)
	}

	if pending, _ := details["pending"].([]string); !slices.Equal(pending, []string{"app", "app_follower_1", "app_follower_2"}) {
		t.Fatalf("post-DROP save pending = %v", details["pending"])
	}

	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("the DROP did not precede the save failure: %v", h.pg.execs)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("post-DROP save failure applied configuration: saves=%d apply=%d", len(h.saves), len(h.applyCalls))
	}
}

func TestExtAddWithFollowersAlreadyEnabledSaveFailureKeepsOldIntentTrue(t *testing.T) {
	cfg := extFollowerCfg(t)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"pg_cron"}
	}

	h := newExtHarness(t, cfg)
	h.journalConverged = false
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	h.saveErrAt[1] = errors.New("disk full")

	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}

	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("already-enabled save failure = %v", err)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != true || details["packages_may_remain"] != true ||
		details["incoming_history_retained"] != true || details["config_rollback"] != "not_needed" {
		t.Fatalf("already-enabled save details = %v", details)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("already-enabled save failure had runtime effects: saves=%d apply=%d exec=%d",
			len(h.saves), len(h.applyCalls), len(h.pg.execs))
	}
}
