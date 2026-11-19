package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func captureBoundaryHistory(t *testing.T) backup.ExtensionRequirements {
	t.Helper()

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	hist, ok := recs.ExtensionRequirementsFor("app-abc123")
	if !ok {
		t.Fatalf("no requirement history for app-abc123")
	}

	return hist
}

func captureBoundaryVariants(reqs []backup.ExtensionRequirement, name string) []catalog.Extension {
	var out []catalog.Extension

	for _, req := range reqs {
		if req.Name == name {
			out = append(out, req.Extension)
		}
	}

	return out
}

func captureBoundaryHas(reqs []backup.ExtensionRequirement, name string) bool {
	return len(captureBoundaryVariants(reqs, name)) > 0
}

func captureBoundaryCLIError(t *testing.T, err error) *output.CLIError {
	t.Helper()

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}

	return cliErr
}

// TestExtAddPrerequisiteFailureRetainsOnlyOutgoingRequirements covers the
// direct-add boundary: a missing file or a nonexistent APT package stops
// before the proposed intent is saved, so the stanza keeps its valid outgoing
// history and gains no speculative incoming provider.
func assertCapturePrerequisiteFailure(t *testing.T, h *extHarness, err error) {
	t.Helper()

	if err == nil || extReason(t, err) != "package_install_failed" {
		t.Fatalf("prerequisite failure = %v", err)
	}

	cliErr := captureBoundaryCLIError(t, err)
	if cliErr.Code != output.CodePartialFailure || cliErr.ExitCode != output.ExitStep {
		t.Fatalf("prerequisite failure code = %s/%d", cliErr.Code, cliErr.ExitCode)
	}

	if !strings.Contains(cliErr.Hint, "may have partially run") ||
		!strings.Contains(cliErr.Hint, "packages are not removed") ||
		!strings.Contains(cliErr.Hint, "did not request a restart or run extension SQL") ||
		strings.Contains(cliErr.Hint, "restored") || strings.Contains(cliErr.Hint, "Nothing was changed") {
		t.Fatalf("prerequisite hint = %q", cliErr.Hint)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != false || details["config_rollback"] != "not_needed" ||
		details["packages_may_remain"] != true {
		t.Fatalf("details = %+v", details)
	}

	if h.prepareCalls != 1 || len(h.saves) != 0 || len(h.applyCalls) != 0 ||
		len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("prerequisite failure effects: prepare=%d saves=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func assertOutgoingRequirementsRetained(t *testing.T, reqs []backup.ExtensionRequirement, seededAt time.Time) {
	t.Helper()

	if got := captureBoundaryVariants(reqs, "hypopg"); len(got) != 0 {
		t.Fatalf("failed add retained incoming providers: %+v", got)
	}

	for _, name := range []string{"pg_stat_statements", "auto_explain", "pg_trgm", "citext"} {
		if !captureBoundaryHas(reqs, name) {
			t.Fatalf("outgoing requirement %q was lost: %+v", name, reqs)
		}
	}

	pgss := captureBoundaryVariants(reqs, "pg_stat_statements")
	if len(pgss) != 1 || pgss[0].ExtName != "pg_stat_statements" {
		t.Fatalf("SQL default requirement = %+v", pgss)
	}

	auto := captureBoundaryVariants(reqs, "auto_explain")
	if len(auto) != 1 || auto[0].ExtName != "" || auto[0].Preload != "auto_explain" {
		t.Fatalf("preload-only default requirement = %+v", auto)
	}

	pgtrgm := captureBoundaryVariants(reqs, "pg_trgm")
	if len(pgtrgm) != 1 || pgtrgm[0].ExtName != "pg_trgm" {
		t.Fatalf("outgoing SQL requirement = %+v", pgtrgm)
	}

	seeded := captureBoundaryVariants(reqs, "citext")
	if len(seeded) != 1 {
		t.Fatalf("seeded requirement = %+v", seeded)
	}

	for _, req := range reqs {
		if req.Name == "citext" && req.Since != seededAt.Format(time.RFC3339) {
			t.Fatalf("existing history timestamp changed: %+v", req)
		}
	}
}

func TestExtAddPrerequisiteFailureRetainsOnlyOutgoingRequirements(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  map[string]any
		failure string
	}{
		{
			name: "missing files",
			params: map[string]any{
				"name": "hypopg", "db": "app", "preinstalled": true,
				"copy_safety": "safe", "standby_safe": true,
			},
			failure: "control file for hypopg is missing",
		},
		{
			name: "nonexistent apt package",
			params: map[string]any{
				"name": "hypopg", "db": "app", "package": "postgresql-18-hypopg-nonexistent",
				"copy_safety": "safe", "standby_safe": true,
			},
			failure: "postgresql-18-hypopg-nonexistent has no installation candidate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extTestCfg("pg_trgm")
			h := newExtHarness(t, cfg)
			h.prepareErr = errors.New(tc.failure)

			seededAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)

			citext, err := cfg.ExtensionResolver().Resolve("citext")
			if err != nil {
				t.Fatal(err)
			}

			if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{citext}, seededAt); err != nil {
				t.Fatal(err)
			}

			_, err = handleExtAdd(context.Background(), &Env{TrustedHostAdmin: true, Cluster: cfg, Req: extReq(t, tc.params)})
			assertCapturePrerequisiteFailure(t, h, err)
			assertOutgoingRequirementsRetained(t, captureBoundaryHistory(t).Requirements, seededAt)
		})
	}
}

// TestExtAddSequentialFailedProvidersLeaveNoIncomingConflict is the EFIN12
// regression: two failed proposals with different providers on a fresh stanza
// must not retain either incoming provider, so a later copy plan has no
// provider conflict.
func TestExtAddSequentialFailedProvidersLeaveNoIncomingConflict(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	h.prepareErr = errors.New("extension prerequisite is unavailable")

	for _, params := range []map[string]any{
		{
			"name": "hypopg", "db": "app", "package": "postgresql-18-hypopg-nonexistent",
			"copy_safety": "safe", "standby_safe": true,
		},
		{
			"name": "hypopg", "db": "app", "preinstalled": true,
			"copy_safety": "safe", "standby_safe": true,
		},
	} {
		_, err := handleExtAdd(context.Background(), &Env{TrustedHostAdmin: true, Cluster: cfg, Req: extReq(t, params)})
		if err == nil || extReason(t, err) != "package_install_failed" {
			t.Fatalf("sequential proposal failure = %v", err)
		}
	}

	reqs := captureBoundaryHistory(t).Requirements
	if got := captureBoundaryVariants(reqs, "hypopg"); len(got) != 0 {
		t.Fatalf("failed proposals retained incoming providers: %+v", got)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backup.MergeExtensionRequirements(recs.RetainedExtensions("app-abc123"), nil); err != nil {
		t.Fatalf("retained history still conflicts: %v", err)
	}

	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("failed proposals had intent or runtime effects: saves=%d apply=%d exec=%d",
			len(h.saves), len(h.applyCalls), len(h.pg.execs))
	}
}

// TestExtAddFailedProposalKeepsExistingProviderHistory proves the fix does not
// prune a stanza's earlier provider history to avoid a conflict: an existing
// preinstalled provider remains when a later APT proposal fails.
func TestExtAddFailedProposalKeepsExistingProviderHistory(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	h.prepareErr = errors.New("postgresql-18-hypopg-nonexistent has no installation candidate")

	preinstalledCfg := cfg.Clone()
	preinstalledCfg.Extensions = map[string]config.ExtensionDefinition{
		"hypopg": {
			Source:      catalog.SourcePreinstalled,
			CopySafety:  new(catalog.CopySafetySafe),
			StandbySafe: new(true),
		},
	}

	preinstalled, err := preinstalledCfg.ExtensionResolver().Resolve("hypopg")
	if err != nil {
		t.Fatal(err)
	}

	seededAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{preinstalled}, seededAt); err != nil {
		t.Fatal(err)
	}

	params := map[string]any{
		"name": "hypopg", "db": "app", "package": "postgresql-18-hypopg-nonexistent",
		"copy_safety": "safe", "standby_safe": true,
	}

	_, err = handleExtAdd(context.Background(), &Env{TrustedHostAdmin: true, Cluster: cfg, Req: extReq(t, params)})
	if err == nil || extReason(t, err) != "package_install_failed" {
		t.Fatalf("failed add = %v", err)
	}

	reqs := captureBoundaryHistory(t).Requirements

	got := captureBoundaryVariants(reqs, "hypopg")
	if len(got) != 1 || got[0].Source != catalog.SourcePreinstalled || got[0].Package != "" {
		t.Fatalf("existing provider history = %+v", got)
	}

	for _, req := range reqs {
		if req.Name == "hypopg" && req.Since != seededAt.Format(time.RFC3339) {
			t.Fatalf("existing provider timestamp changed: %+v", req)
		}
	}
}

// TestExtAddOutgoingCaptureFailurePreventsPreparation proves requirement
// durability is the first effect: a failed outgoing snapshot does no package,
// intent, config, restart or SQL work.
func TestExtAddOutgoingCaptureFailurePreventsPreparation(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)

	citext, err := cfg.ExtensionResolver().Resolve("citext")
	if err != nil {
		t.Fatal(err)
	}

	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{citext}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	oldOutgoing, oldIncoming := extensionCaptureOutgoing, extensionCaptureIncoming
	extensionCaptureOutgoing = func(config.Cluster) error { return errors.New("records unavailable") }
	incomingCalls := 0
	extensionCaptureIncoming = func(config.Cluster) error {
		incomingCalls++
		return nil
	}

	t.Cleanup(func() {
		extensionCaptureOutgoing, extensionCaptureIncoming = oldOutgoing, oldIncoming
	})

	_, err = handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("outgoing capture failure = %v", err)
	}

	if cliErr := captureBoundaryCLIError(t, err); !strings.Contains(cliErr.Hint, "outgoing requirement snapshots") ||
		strings.Contains(cliErr.Hint, "Nothing was changed") {
		t.Fatalf("outgoing capture hint = %q", cliErr.Hint)
	}

	if h.prepareCalls != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 ||
		len(h.pg.execs) != 0 || h.restartCalls != 0 || incomingCalls != 0 {
		t.Fatalf("outgoing capture failure effects: prepare=%d saves=%d apply=%d exec=%d restart=%d incoming=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls, incomingCalls)
	}

	if reqs := captureBoundaryHistory(t).Requirements; !captureBoundaryHas(reqs, "citext") {
		t.Fatalf("outgoing capture failure erased existing history: %+v", reqs)
	}
}

// TestExtAddIncomingCaptureFailureStopsBeforeIntentAndRuntime covers the
// post-preparation boundary. Package work has already run, so the error must
// not claim that nothing changed; cluster.toml, restart and SQL stay untouched.
func TestExtAddIncomingCaptureFailureStopsBeforeIntentAndRuntime(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	oldIncoming := extensionCaptureIncoming
	extensionCaptureIncoming = func(config.Cluster) error { return errors.New("records unavailable") }

	t.Cleanup(func() { extensionCaptureIncoming = oldIncoming })

	_, err := handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("incoming capture failure = %v", err)
	}

	cliErr := captureBoundaryCLIError(t, err)
	if strings.Contains(cliErr.Error(), "Nothing was changed") || strings.Contains(cliErr.Hint, "Nothing was changed") {
		t.Fatalf("incoming capture failure claimed no effect: message=%q hint=%q", cliErr.Message, cliErr.Hint)
	}

	if !strings.Contains(cliErr.Hint, "may already be installed") {
		t.Fatalf("incoming capture hint = %q", cliErr.Hint)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != false || details["config_rollback"] != "not_needed" ||
		details["packages_may_remain"] != true {
		t.Fatalf("details = %+v", details)
	}

	if h.prepareCalls != 1 || len(h.saves) != 0 || len(h.applyCalls) != 0 ||
		len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("incoming capture failure effects: prepare=%d saves=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}

	reqs := captureBoundaryHistory(t).Requirements
	if got := captureBoundaryVariants(reqs, "pg_trgm"); len(got) != 0 {
		t.Fatalf("incoming capture failure retained the proposed provider: %+v", got)
	}

	if !captureBoundaryHas(reqs, "pg_stat_statements") || !captureBoundaryHas(reqs, "auto_explain") {
		t.Fatalf("outgoing defaults were not durable: %+v", reqs)
	}
}

// TestExtAddIncomingCaptureFailureReportsExistingIntent keeps intent_saved
// truthful when cluster.toml already enables the extension: the durable
// desired state was unchanged, exactly as a prerequisite failure reports.
func TestExtAddIncomingCaptureFailureReportsExistingIntent(t *testing.T) {
	cfg := extTestCfg("pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	oldIncoming := extensionCaptureIncoming
	extensionCaptureIncoming = func(config.Cluster) error { return errors.New("records unavailable") }

	t.Cleanup(func() { extensionCaptureIncoming = oldIncoming })

	_, err := handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("incoming capture failure = %v", err)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != true || details["config_rollback"] != "not_needed" ||
		details["packages_may_remain"] != true {
		t.Fatalf("details = %+v", details)
	}

	if h.prepareCalls != 1 || len(h.saves) != 0 || len(h.applyCalls) != 0 ||
		len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("incoming capture failure effects: prepare=%d saves=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestExtAddIncomingCapturePartialWriteIsDisclosed(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	oldIncoming := extensionCaptureIncoming
	extensionCaptureIncoming = func(incoming config.Cluster) error {
		ext, err := incoming.ExtensionResolver().Resolve("pg_trgm")
		if err != nil {
			return err
		}
		// Model a successful stanza update followed by a later capture failure.
		// The real durable snapshot must survive; it is not an atomic rollback.
		if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{ext}, time.Now().UTC()); err != nil {
			return err
		}

		return errors.New("a later stanza capture failed")
	}

	t.Cleanup(func() { extensionCaptureIncoming = oldIncoming })

	_, err := handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
	if err == nil || extReason(t, err) != "extension_history_capture_failed" {
		t.Fatalf("partial incoming capture = %v", err)
	}

	if extDetails(t, err)["incoming_history_may_remain"] != true {
		t.Fatalf("partial history was not disclosed: %+v", extDetails(t, err))
	}

	cliErr := captureBoundaryCLIError(t, err)
	if !strings.Contains(strings.ToLower(cliErr.Hint), "incoming") ||
		!strings.Contains(strings.ToLower(cliErr.Hint), "retain") ||
		strings.Contains(cliErr.Hint, "Nothing was changed") {
		t.Fatalf("partial capture hint = %q", cliErr.Hint)
	}

	if !captureBoundaryHas(captureBoundaryHistory(t).Requirements, "pg_trgm") {
		t.Fatal("valid partial incoming history was erased")
	}

	if h.prepareCalls != 1 || len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("partial capture crossed the intent/runtime boundary: prepare=%d save=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

// TestExtAddIntentSaveFailureAfterPreparationIsPartial covers a save failure
// after the incoming snapshot was written. The package work already happened,
// so the error must disclose that and must not run config, restart or SQL.
func TestExtAddIntentSaveFailureAfterPreparationIsPartial(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	h.saveErrAt[1] = errors.New("disk full")

	_, err := handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("intent save failure = %v", err)
	}

	cliErr := captureBoundaryCLIError(t, err)
	if cliErr.Code != output.CodePartialFailure || cliErr.ExitCode != output.ExitStep {
		t.Fatalf("intent save failure code = %s/%d", cliErr.Code, cliErr.ExitCode)
	}

	if !strings.Contains(cliErr.Hint, "may already be installed") ||
		!strings.Contains(cliErr.Hint, "did not request a restart or run extension SQL") ||
		strings.Contains(cliErr.Hint, "Nothing was changed") {
		t.Fatalf("intent save hint = %q", cliErr.Hint)
	}

	details := extDetails(t, err)
	if details["intent_saved"] != false || details["config_rollback"] != "not_needed" ||
		details["packages_may_remain"] != true || details["incoming_history_retained"] != true {
		t.Fatalf("details = %+v", details)
	}

	if h.prepareCalls != 1 || len(h.saveAttempts) != 1 || len(h.saves) != 0 ||
		len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("intent save failure effects: prepare=%d attempts=%d saves=%d apply=%d exec=%d restart=%d",
			h.prepareCalls, len(h.saveAttempts), len(h.saves), len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}

	reqs := captureBoundaryHistory(t).Requirements
	if got := captureBoundaryVariants(reqs, "pg_trgm"); len(got) != 1 || got[0].ExtName != "pg_trgm" {
		t.Fatalf("incoming requirement was not retained: %+v", got)
	}
}

// TestExtAddLaterFailureRetainsIncomingHistory proves conservative retention:
// once preparation succeeded and the incoming snapshot was written, a later
// config or SQL failure keeps it instead of rewriting history.
func TestExtAddLaterFailureRetainsIncomingHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantReason string
		failOn     func(engine.Options) bool
	}{
		{
			name:       "config apply failure",
			wantReason: "extension_apply_failed",
			failOn: func(opts engine.Options) bool {
				return len(opts.Only) == 1 && opts.Only[0] == steps.ConfigStepID("app")
			},
		},
		{
			name:       "SQL failure",
			wantReason: "extension_sql_failed",
			failOn: func(opts engine.Options) bool {
				return len(opts.Only) == 1 && opts.Only[0] == steps.RolesStepID("app")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extTestCfg()
			h := newExtHarness(t, cfg)
			h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
				return map[string]extensionInstalledVersion{}, nil
			}
			h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
				if tc.failOn(opts) {
					return engine.ApplyData{}, errors.New("apply stopped")
				}

				return engine.ApplyData{}, nil
			}

			_, err := handleExtAdd(context.Background(), &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})})
			if err == nil || extReason(t, err) != tc.wantReason {
				t.Fatalf("later failure = %v", err)
			}

			if len(h.saves) != 1 {
				t.Fatalf("intent saves = %d, want 1", len(h.saves))
			}

			reqs := captureBoundaryHistory(t).Requirements
			if got := captureBoundaryVariants(reqs, "pg_trgm"); len(got) != 1 || got[0].ExtName != "pg_trgm" {
				t.Fatalf("incoming requirement was not retained: %+v", got)
			}
		})
	}
}
