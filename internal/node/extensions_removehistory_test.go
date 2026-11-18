package node

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

func removeHistoryCLIError(t *testing.T, err error) *output.CLIError {
	t.Helper()

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}

	return cliErr
}

func removeHistoryVariants(t *testing.T, stanza, name string) []catalog.Extension {
	t.Helper()

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	hist, ok := recs.ExtensionRequirementsFor(stanza)
	if !ok {
		t.Fatalf("no requirement history for %s", stanza)
	}

	var out []catalog.Extension

	for _, req := range hist.Requirements {
		if req.Name == name {
			out = append(out, req.Extension)
		}
	}

	return out
}

func removeHistoryDependentsZero(sql string) ([]map[string]string, error) {
	if strings.Contains(sql, "count(*)") {
		return []map[string]string{{"n": "0"}}, nil
	}

	return nil, nil
}

// TestExtRemoveHistoryCaptureFailureReportsPartialState proves a durable
// outgoing snapshot written before a capture error is not hidden by a blanket
// "nothing changed": no DROP, reduced intent, configuration or restart runs,
// and the valid retained metadata stays.
func TestExtRemoveHistoryCaptureFailureReportsPartialState(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("1.6", "1.6"), nil
	}
	h.pg.queryFn = removeHistoryDependentsZero

	oldCapture := extensionCaptureRequirements
	extensionCaptureRequirements = func(_ context.Context, oldCfg config.Cluster, oldSpec config.ClusterSpec, _ config.Cluster, _ config.ClusterSpec, _ string) error {
		ext, err := oldCfg.ExtensionResolver().Resolve("pg_trgm")
		if err != nil {
			return err
		}

		if err := backup.RecordExtensionRequirements(oldSpec.Stanza, oldSpec.Name, []catalog.Extension{ext}, time.Now().UTC()); err != nil {
			return err
		}

		return errors.New("history store failed after retaining the outgoing snapshot")
	}

	t.Cleanup(func() { extensionCaptureRequirements = oldCapture })

	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

	_, err := extensionRemoveExecute(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("capture failure = %v", err)
	}

	cliErr := removeHistoryCLIError(t, err)
	if cliErr.Code != output.CodePartialFailure || cliErr.ExitCode != output.ExitStep {
		t.Fatalf("capture failure code = %s/%d", cliErr.Code, cliErr.ExitCode)
	}

	if strings.Contains(cliErr.Hint, "Nothing was changed") ||
		!strings.Contains(cliErr.Hint, "history may already have been retained") {
		t.Fatalf("capture failure hint = %q", cliErr.Hint)
	}

	details := extDetails(t, err)
	if details["outgoing_history_may_remain"] != true || details["incoming_history_may_remain"] != true ||
		details["sql_dropped"] != false || details["intent_saved"] != false ||
		details["reduced_intent_saved"] != false || details["configuration_applied"] != false ||
		details["restarted"] != false || details["restart_required"] != false {
		t.Fatalf("capture failure details = %+v", details)
	}

	if retry, _ := details["retry"].(string); !strings.Contains(retry, "vaire ext remove pg_trgm --db app --confirm app") {
		t.Fatalf("capture failure retry = %q", retry)
	}

	if pending, ok := details["pending"].([]string); !ok || !slices.Equal(pending, []string{"app"}) {
		t.Fatalf("capture failure pending = %#v", details["pending"])
	}

	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 || h.restartCalls != 0 {
		t.Fatalf("capture failure effects: exec=%d saves=%d apply=%d restart=%d",
			len(h.pg.execs), len(h.saves), len(h.applyCalls), h.restartCalls)
	}

	retained := removeHistoryVariants(t, "app-abc123", "pg_trgm")
	if len(retained) != 1 || retained[0].ExtName != "pg_trgm" {
		t.Fatalf("durable outgoing metadata was lost: %+v", retained)
	}
}

// TestExtRemovePostDropSaveFailureReportsActualState proves the Save path after
// a real DROP reports the actual SQL stage, the unchanged reduced intent, the
// pending target and the supported retry without applying config or restart.
func TestExtRemovePostDropSaveFailureReportsActualState(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("1.6", "1.6"), nil
	}
	h.pg.queryFn = removeHistoryDependentsZero
	h.saveErrAt[1] = errors.New("disk full")

	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

	_, err := extensionRemoveExecute(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("save failure = %v", err)
	}

	cliErr := removeHistoryCLIError(t, err)
	if cliErr.Code != output.CodePartialFailure || cliErr.ExitCode != output.ExitStep ||
		!strings.Contains(cliErr.Message, "dropping pg_trgm's SQL succeeded") ||
		!strings.Contains(cliErr.Hint, "SQL is dropped") {
		t.Fatalf("save failure = %s/%d %q / %q", cliErr.Code, cliErr.ExitCode, cliErr.Message, cliErr.Hint)
	}

	details := extDetails(t, err)
	if details["sql_dropped"] != true || details["intent_saved"] != false ||
		details["reduced_intent_saved"] != false || details["configuration_applied"] != false ||
		details["restarted"] != false || details["restart_required"] != false {
		t.Fatalf("save failure details = %+v", details)
	}

	if retry, _ := details["retry"].(string); !strings.Contains(retry, "vaire ext remove pg_trgm --db app --confirm app") {
		t.Fatalf("save failure retry = %q", retry)
	}

	if pending, ok := details["pending"].([]string); !ok || !slices.Equal(pending, []string{"app"}) {
		t.Fatalf("save failure pending = %#v", details["pending"])
	}

	if len(h.pg.execs) != 1 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") ||
		len(h.saves) != 0 || len(h.applyCalls) != 0 || h.restartCalls != 0 {
		t.Fatalf("save failure effects: exec=%v saves=%d apply=%d restart=%d",
			h.pg.execs, len(h.saves), len(h.applyCalls), h.restartCalls)
	}
}

// TestExtRemoveSaveFailureWithoutDropReportsNoSQLStage keeps sql_dropped
// truthful when cluster.toml enables the extension but its SQL is already
// absent: no DROP ran, so the failure must not claim one did.
func TestExtRemoveSaveFailureWithoutDropReportsNoSQLStage(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.saveErrAt[1] = errors.New("disk full")

	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}

	_, err := extensionRemoveExecute(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("save failure = %v", err)
	}

	cliErr := removeHistoryCLIError(t, err)
	if strings.Contains(cliErr.Message, "dropping pg_trgm's SQL succeeded") ||
		strings.Contains(cliErr.Hint, "SQL is dropped") || !strings.Contains(cliErr.Hint, "No SQL was dropped") {
		t.Fatalf("save failure = %q / %q", cliErr.Message, cliErr.Hint)
	}

	details := extDetails(t, err)

	pending, ok := details["pending"].([]string)
	if !ok {
		t.Fatalf("pending has unexpected type %T", details["pending"])
	}

	if details["sql_dropped"] != false || details["intent_saved"] != false ||
		len(pending) == 0 || pending[0] != "app" {
		t.Fatalf("save failure details = %+v", details)
	}

	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 || h.restartCalls != 0 {
		t.Fatalf("save failure effects: exec=%v saves=%d apply=%d restart=%d",
			h.pg.execs, len(h.saves), len(h.applyCalls), h.restartCalls)
	}
}

// TestExtRemovePostDropSaveFailureKeepsRestartState proves the partial shape
// carries the expected restart requirement and tokenized retry after a DROP.
func TestExtRemovePostDropSaveFailureKeepsRestartState(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_cron")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	h.pg.queryFn = removeHistoryDependentsZero
	h.saveErrAt[1] = errors.New("disk full")

	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"}, "app", "app/restart")}

	_, err := extensionRemoveExecute(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("save failure = %v", err)
	}

	details := extDetails(t, err)
	if details["restart_required"] != true || details["sql_dropped"] != true {
		t.Fatalf("save failure details = %+v", details)
	}

	if retry, _ := details["retry"].(string); !strings.Contains(retry, "app/restart") {
		t.Fatalf("save failure retry = %q", retry)
	}

	if len(h.applyCalls) != 0 || h.restartCalls != 0 {
		t.Fatalf("save failure applied runtime: apply=%d restart=%d", len(h.applyCalls), h.restartCalls)
	}
}

// TestExtRemoveIntentFailureSharesEncodeSaveShape proves the defensive Encode
// failure and the injectable Save failure return the same typed partial
// contract. A validated config.Cluster has no public normal-state fixture that
// makes Encode fail, so the actual Save path plus this shared helper test are
// the reachability proof.
func TestExtRemoveIntentFailureSharesEncodeSaveShape(t *testing.T) {
	cfg := extTestCfg("pg_trgm")
	spec := cfg.Clusters[0]

	ext, err := cfg.ExtensionResolver().Resolve("pg_trgm")
	if err != nil {
		t.Fatal(err)
	}

	retry := "vaire ext remove pg_trgm --db app --confirm app"
	encodeErr := extensionRemoveIntentFailure("encoding the reduced extension intent", errors.New("encode failed"), spec, ext, retry, true, true)

	saveErr := extensionRemoveIntentFailure("saving the reduced cluster.toml", errors.New("save failed"), spec, ext, retry, true, true)
	for _, failure := range []error{encodeErr, saveErr} {
		cliErr := removeHistoryCLIError(t, failure)
		if cliErr.Code != output.CodePartialFailure || cliErr.ExitCode != output.ExitStep {
			t.Fatalf("shared failure code = %s/%d", cliErr.Code, cliErr.ExitCode)
		}

		if extReason(t, failure) != "extension_intent_save_failed" {
			t.Fatalf("shared failure reason = %s", extReason(t, failure))
		}
	}

	encodeDetails := extDetails(t, encodeErr)

	saveDetails := extDetails(t, saveErr)
	if !reflect.DeepEqual(encodeDetails, saveDetails) {
		t.Fatalf("Encode/Save details differ:\nencode=%+v\nsave=%+v", encodeDetails, saveDetails)
	}

	pending, ok := saveDetails["pending"].([]string)
	if !ok {
		t.Fatalf("pending has unexpected type %T", saveDetails["pending"])
	}

	if saveDetails["sql_dropped"] != true || saveDetails["intent_saved"] != false ||
		saveDetails["restart_required"] != true || len(pending) == 0 || pending[0] != "app" ||
		saveDetails["retry"] != retry {
		t.Fatalf("shared failure details = %+v", saveDetails)
	}
}
