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
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

type fakeExtPG struct {
	execs   []string
	execErr error
	execFn  func(string)
	queryFn func(string) ([]map[string]string, error)
}

func (f *fakeExtPG) Exec(_ context.Context, _, sql string, _ map[string]string) error {
	f.execs = append(f.execs, sql)
	if f.execFn != nil {
		f.execFn(sql)
	}
	return f.execErr
}

func (f *fakeExtPG) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	if f.queryFn == nil {
		return nil, nil
	}
	return f.queryFn(sql)
}

type extHarness struct {
	cfg     config.Cluster
	raw     []byte
	pg      *fakeExtPG
	records backup.Records

	saveAttempts [][]byte
	saves        [][]byte
	saveErrAt    map[int]error

	prepareCalls int
	prepareErr   error
	prepareCfg   config.Cluster
	prepareSpec  config.ClusterSpec

	applyCalls    []engine.Options
	applyCfg      config.Cluster
	applyFn       func(engine.Options) (engine.ApplyData, error)
	applyWarnings []output.Warning

	installedCalls int
	installedFn    func(call int) (map[string]extensionInstalledVersion, error)

	preloadLoaded  map[string]bool
	preloadPending bool
	preloadErr     error

	pendingSettings     []string
	pendingSettingsErr  error
	pendingRestartsCall int
	checkStepFn         func(string) (string, error)
	checkStepCalls      []string
	journalConverged    bool
	journalErr          error

	runningFn func(config.ClusterSpec) bool

	updatePathCalls int
	updatePath      bool
	updatePathErr   error

	restartCalls int
	restartErr   error

	packageInstalled bool
	packageKnown     bool
	packageErr       string
}

func newExtHarness(t *testing.T, cfg config.Cluster) *extHarness {
	t.Helper()
	h := &extHarness{
		cfg: cfg, raw: []byte("previous"), pg: &fakeExtPG{queryFn: defaultExtensionSettingsFake},
		saveErrAt: map[int]error{}, records: backup.Records{},
		preloadLoaded:    map[string]bool{"pg_stat_statements": true, "auto_explain": true},
		packageInstalled: true, packageKnown: true, journalConverged: true,
	}
	oldLoad, oldSave, oldRecords := extensionLoad, extensionSave, extensionRecords
	oldPrepare, oldApply := extensionPrepareStart, extensionApply
	oldPG, oldRunning := extensionPG, extensionClusterRunning
	oldPreload, oldInstalled := extensionPreloadFacts, extensionInstalled
	oldUpdate, oldPackage := extensionUpdatePath, extensionPackageInstalled
	oldRestart, oldClear := extensionRestart, extensionClearDeferred
	oldCheck, oldPending := extensionCheckStep, extensionPendingRestarts
	oldJournal := extensionJournalConverged
	oldDetect := detectHost
	oldRecordsPath := backup.RecordsPath
	oldAcquire := extensionAcquireClusters
	extensionAcquireClusters = func(context.Context, []string, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	// Node lifecycle tests model host/PG effects, not a real follower catalog.
	// The planner's live class is still exercised; tests that need installed
	// SQL extras or a catalog failure replace this seam themselves.
	oldFollowerSQL := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	oldGuardInstance := steps.SetGuardInstanceForTest(func() (string, error) { return "test-instance", nil })
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() {
		extensionLoad, extensionSave, extensionRecords = oldLoad, oldSave, oldRecords
		extensionPrepareStart, extensionApply = oldPrepare, oldApply
		extensionPG, extensionClusterRunning = oldPG, oldRunning
		extensionPreloadFacts, extensionInstalled = oldPreload, oldInstalled
		extensionUpdatePath, extensionPackageInstalled = oldUpdate, oldPackage
		extensionRestart, extensionClearDeferred = oldRestart, oldClear
		extensionCheckStep, extensionPendingRestarts = oldCheck, oldPending
		extensionJournalConverged = oldJournal
		detectHost = oldDetect
		extensionAcquireClusters = oldAcquire
		oldFollowerSQL()
		oldGuardInstance()
		backup.RecordsPath = oldRecordsPath
	})

	extensionLoad = func() (config.Cluster, []byte, error) { return h.cfg, h.raw, nil }
	extensionSave = func(raw []byte) error {
		h.saveAttempts = append(h.saveAttempts, append([]byte(nil), raw...))
		if err := h.saveErrAt[len(h.saveAttempts)]; err != nil {
			return err
		}
		h.saves = append(h.saves, append([]byte(nil), raw...))
		return nil
	}
	extensionRecords = func() (backup.Records, error) { return h.records, nil }
	extensionPrepareStart = func(_ context.Context, cfg config.Cluster, spec config.ClusterSpec, _ []steps.ExtensionPrerequisite, _ engine.Runtime, _ engine.Reporter) error {
		h.prepareCalls++
		h.prepareCfg, h.prepareSpec = cfg, spec
		return h.prepareErr
	}
	extensionApply = func(_ context.Context, cfg config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		h.applyCalls = append(h.applyCalls, opts)
		h.applyCfg = cfg
		if h.applyFn != nil {
			data, err := h.applyFn(opts)
			return data, h.applyWarnings, err
		}
		return engine.ApplyData{}, h.applyWarnings, nil
	}
	extensionPG = func(config.ClusterSpec) sqlRunner { return h.pg }
	extensionClusterRunning = func(spec config.ClusterSpec) bool {
		if h.runningFn != nil {
			return h.runningFn(spec)
		}
		return true
	}
	extensionPreloadFacts = func(context.Context, config.ClusterSpec) (map[string]bool, bool, error) {
		return h.preloadLoaded, h.preloadPending, h.preloadErr
	}
	extensionInstalled = func(context.Context, config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		h.installedCalls++
		if h.installedFn != nil {
			return h.installedFn(h.installedCalls)
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	extensionUpdatePath = func(context.Context, config.ClusterSpec, string, string, string) (bool, error) {
		h.updatePathCalls++
		return h.updatePath, h.updatePathErr
	}
	extensionPackageInstalled = func(context.Context, string) (bool, bool, string) {
		return h.packageInstalled, h.packageKnown, h.packageErr
	}
	extensionRestart = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime, engine.Reporter) (time.Duration, error) {
		h.restartCalls++
		return 0, h.restartErr
	}
	extensionClearDeferred = func(...string) {}
	extensionPendingRestarts = func(context.Context, config.ClusterSpec) ([]string, error) {
		h.pendingRestartsCall++
		return slices.Clone(h.pendingSettings), h.pendingSettingsErr
	}
	extensionCheckStep = func(_ context.Context, _ config.Cluster, _ engine.Runtime, id string) (string, error) {
		h.checkStepCalls = append(h.checkStepCalls, id)
		if h.checkStepFn != nil {
			return h.checkStepFn(id)
		}
		return "", nil
	}
	extensionJournalConverged = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime) (bool, error) {
		return h.journalConverged, h.journalErr
	}
	detectHost = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	return h
}

func extTestCfg(extensions ...string) config.Cluster {
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small",
		StatementTimeout: "60s", Extensions: extensions,
	}}
	return cfg
}

func extAddCluster(name string, port int, extensions ...string) config.ClusterSpec {
	return config.ClusterSpec{Name: name, Node: "db1", Port: port, Stanza: name + "-abc123", Size: "small",
		StatementTimeout: "60s", Extensions: extensions}
}

func extFollowerCluster(name, source string, port int) config.ClusterSpec {
	return config.ClusterSpec{Name: name, Node: "db1", Port: port, Size: "small", StatementTimeout: "60s", Follows: source,
		Extensions: []string{"pg_stat_statements"}}
}

func extReq(t *testing.T, params any, confirms ...string) rpc.Request {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	req := rpc.Request{Params: raw}
	if len(confirms) > 0 {
		req.Confirm = confirms[0]
		req.Confirms = confirms[1:]
	}
	return req
}

func extReason(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

func extDetails(t *testing.T, err error) map[string]any {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}
	details, _ := cliErr.Details.(map[string]any)
	return details
}

func installedVersion(version, def string) map[string]extensionInstalledVersion {
	return map[string]extensionInstalledVersion{"pg_trgm": {Name: "pg_trgm", Version: version, Default: def}}
}

func TestExtAddMissingRestartTokenHasZeroEffects(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app/restart" {
		t.Fatalf("missing restart token: %v", err)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("missing token had effects: saves=%d prepare=%d apply=%d exec=%d restart=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestExtAddDryRunHasZeroEffects(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	env := &Env{Cluster: h.cfg, Req: rpc.Request{Params: mustJSON(t, map[string]any{"name": "pg_cron", "db": "app"}), DryRun: true}}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.Data.(extAddResult)
	if !ok || !data.DryRun || !data.RestartRequired || !slices.Contains(data.RequiredConfirmTokens, "app/restart") {
		t.Fatalf("dry run result = %+v", res.Data)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("dry run had effects: saves=%d prepare=%d apply=%d exec=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs))
	}
}

func TestExtAddDryRunShowsUpdate(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.5", "1.6"), nil }
	env := &Env{Cluster: h.cfg, Req: rpc.Request{Params: mustJSON(t, map[string]any{"name": "pg_trgm", "db": "app"}), DryRun: true}}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.UpdateAvailable || data.Version != "1.5" || data.DefaultVersion != "1.6" {
		t.Fatalf("result = %+v", data)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("dry run had effects: saves=%d prepare=%d apply=%d exec=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs))
	}

	h = newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("2.0", "1.0"), nil }
	env = &Env{Cluster: h.cfg, Req: rpc.Request{Params: mustJSON(t, map[string]any{"name": "pg_trgm", "db": "app"}), DryRun: true}}
	res, err = handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data = res.Data.(extAddResult)
	if !data.Downgrade || data.UpdateAvailable {
		t.Fatalf("downgrade preview = %+v", data)
	}

	h = newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("v2", "v1"), nil }
	env = &Env{Cluster: h.cfg, Req: rpc.Request{Params: mustJSON(t, map[string]any{"name": "pg_trgm", "db": "app"}), DryRun: true}}
	res, err = handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data = res.Data.(extAddResult)
	if !data.DirectionUnknown || data.Downgrade {
		t.Fatalf("nonnumeric preview = %+v", data)
	}
}

func TestExtAddStandaloneCreatesSQL(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return installedVersion("1.6", "1.6"), nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if data.Version != "1.6" || data.AlreadyEnabled || data.DescriptorSaved || data.RestartRequired || !data.Created {
		t.Fatalf("result = %+v", data)
	}
	if len(h.saves) != 1 || h.prepareCalls != 1 || len(h.applyCalls) != 2 {
		t.Fatalf("effects: saves=%d prepare=%d apply=%d", len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
	if h.applyCalls[0].Only[0] != steps.ConfigStepID("app") || h.applyCalls[1].Only[0] != steps.RolesStepID("app") {
		t.Fatalf("apply order = %+v", h.applyCalls)
	}
}

func TestExtAddAliasStoresCanonicalName(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"vector": {Name: "vector", Version: "0.9.0", Default: "0.9.0"}}, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pgvector", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if data := res.Data.(extAddResult); data.Extension != "vector" || data.PackageKept != "postgresql-18-pgvector" {
		t.Fatalf("result = %+v", res.Data)
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.Clusters[0].Extensions, "vector") || slices.Contains(saved.Clusters[0].Extensions, "pgvector") {
		t.Fatalf("saved selection = %v", saved.Clusters[0].Extensions)
	}
}

func TestExtAddCustomDescriptorPersistsOnlyAfterValidationAndAuthorization(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"custom_ext": {Name: "custom_ext", Version: "1.0", Default: "1.0"}}, nil
	}
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	standby := true
	params := map[string]any{
		"name": "custom_ext", "db": "app", "package": "postgresql-18-custom",
		"preload": "custom_lib", "copy_safety": "safe", "standby_safe": standby,
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, params, "app/restart")}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Data.(extAddResult).DescriptorSaved {
		t.Fatal("custom descriptor was not reported as saved")
	}
	if len(h.saves) != 1 {
		t.Fatalf("saves = %d", len(h.saves))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	def, ok := saved.Extensions["custom_ext"]
	if !ok || def.Source != "apt" || def.Package != "postgresql-18-custom" || def.Preload != "custom_lib" ||
		def.CopySafety == nil || *def.CopySafety != "safe" || def.StandbySafe == nil || !*def.StandbySafe {
		t.Fatalf("saved descriptor = %+v", def)
	}
	if !slices.Contains(saved.Clusters[0].Extensions, "custom_ext") {
		t.Fatalf("saved selection = %v", saved.Clusters[0].Extensions)
	}
}

func TestExtAddInvalidDescriptorHasZeroEffects(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "custom_ext", "db": "app", "package": "not a package"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil {
		t.Fatal("invalid package was accepted")
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("invalid descriptor had effects: saves=%d prepare=%d apply=%d exec=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs))
	}
}

func TestExtAddPackageFailureRollsBackIntent(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.prepareErr = errors.New("apt install failed")
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "package_install_failed" {
		t.Fatalf("package failure: %v", err)
	}
	details := extDetails(t, err)
	if details["intent_saved"] != false || details["config_rollback"] != "restored" || details["packages_may_remain"] != true {
		t.Fatalf("details = %+v", details)
	}
	if len(h.saveAttempts) != 2 || len(h.saves) != 2 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("rollback effects: attempts=%d saves=%d apply=%d exec=%d", len(h.saveAttempts), len(h.saves), len(h.applyCalls), len(h.pg.execs))
	}
	if string(h.saveAttempts[1]) != string(h.raw) {
		t.Fatalf("rollback wrote %q, want %q", h.saveAttempts[1], h.raw)
	}
}

func TestExtAddRollbackSaveFailureIsSurfaced(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.prepareErr = errors.New("apt install failed")
	h.saveErrAt[2] = errors.New("disk full")
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil {
		t.Fatal("rollback failure was not reported")
	}
	details := extDetails(t, err)
	if details["intent_saved"] != true || !strings.Contains(details["config_rollback"].(string), "failed") {
		t.Fatalf("details = %+v", details)
	}
}

func TestExtAddDoesNotNoOpWhenPreloadIsMissing(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"auto_explain": true}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_stat_statements", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.PreloadMissing || data.AlreadyEnabled != true {
		t.Fatalf("result = %+v", data)
	}
	if h.prepareCalls != 1 || len(h.applyCalls) != 2 {
		t.Fatalf("missing preload was treated as a no-op: prepare=%d apply=%d", h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtAddDefaultModuleIsNoOp(t *testing.T) {
	cfg := extTestCfg()
	cfg.Clusters[0].Extensions = nil
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_stat_statements", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.AlreadyEnabled || data.Version != "1.12" {
		t.Fatalf("result = %+v", data)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("default module no-op had effects: saves=%d prepare=%d apply=%d", len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtRemoveDefaultModuleRefused(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements"))
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_stat_statements", "db": "app"}, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_default" {
		t.Fatalf("default remove: %v", err)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("default remove had effects: saves=%d apply=%d exec=%v", len(h.saves), len(h.applyCalls), h.pg.execs)
	}
}

func TestExtAddReAddPendingCreatesSQL(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return installedVersion("1.6", "1.6"), nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.AlreadyEnabled || data.Version != "1.6" {
		t.Fatalf("result = %+v", data)
	}
	if len(h.saves) != 0 || h.prepareCalls != 1 || len(h.applyCalls) != 2 {
		t.Fatalf("effects: saves=%d prepare=%d apply=%d", len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtAddPendingPreloadRestartsExactTarget(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_cron"))
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	h.preloadPending = true
	h.pendingSettings = []string{"shared_preload_libraries"}
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"}, "app/restart")}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.Restarted || !data.PendingRestart {
		t.Fatalf("result = %+v", data)
	}
	if h.restartCalls != 1 {
		t.Fatalf("restart calls = %d", h.restartCalls)
	}
}

func TestExtAddSupportedUpdateReportsActualVersion(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		switch call {
		case 1, 2:
			return installedVersion("1.5", "1.6"), nil
		default:
			return installedVersion("1.6", "1.6"), nil
		}
	}
	h.updatePath = true
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.Updated || data.PreviousVersion != "1.5" || data.Version != "1.6" {
		t.Fatalf("result = %+v", data)
	}
	if len(h.pg.execs) != 1 || !strings.Contains(h.pg.execs[0], `ALTER EXTENSION "pg_trgm" UPDATE TO '1.6'`) {
		t.Fatalf("execs = %v", h.pg.execs)
	}
}

func TestExtAddRolesIncompleteIsReportedHonestly(t *testing.T) {
	h := newExtHarness(t, extTestCfg())
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if len(opts.Only) == 1 && opts.Only[0] == steps.RolesStepID("app") {
			return engine.ApplyData{Steps: []engine.ApplyStep{{ID: steps.RolesStepID("app"), State: engine.StatusIncomplete}}}, nil
		}
		return engine.ApplyData{}, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_roles_incomplete" {
		t.Fatalf("roles incomplete: %v", err)
	}
}

func TestExtAddUnsupportedUpdateIsRefused(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("1.5", "1.6"), nil
	}
	h.updatePath = false
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_update_unsupported" {
		t.Fatalf("unsupported update: %v", err)
	}
	if len(h.pg.execs) != 0 {
		t.Fatalf("unsupported update ran SQL: %v", h.pg.execs)
	}
}

func TestExtAddDowngradeIsRefused(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("2.0", "1.0"), nil
	}
	h.updatePath = true
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_downgrade_refused" {
		t.Fatalf("downgrade: %v", err)
	}
	if h.updatePathCalls != 0 || len(h.pg.execs) != 0 || len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("downgrade had effects: path=%d exec=%v saves=%d prepare=%d apply=%d", h.updatePathCalls, h.pg.execs, len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtRemoveMissingDBTokenHasZeroEffects(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app" {
		t.Fatalf("missing db token: %v", err)
	}
	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("missing token had effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}

func TestExtRemoveProtectedRefused(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	cfg.Clusters[0].Protected = true
	h := newExtHarness(t, cfg)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "database_protected" {
		t.Fatalf("protected remove: %v", err)
	}
	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("protected remove had effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}

func TestExtRemoveDependentsPreviewAndCascade(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return installedVersion("1.6", "1.6"), nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		switch {
		case strings.Contains(sql, "count(*)"):
			return []map[string]string{{"n": "51"}}, nil
		case strings.Contains(sql, "pg_describe_object"):
			rows := make([]map[string]string, 51)
			for i := range rows {
				rows[i] = map[string]string{"object": "object_" + string(rune('a'+i%26))}
			}
			return rows, nil
		default:
			return nil, nil
		}
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_in_use" {
		t.Fatalf("dependents without cascade: %v", err)
	}
	details := extDetails(t, err)
	if details["dependents_total"] != 51 || details["truncated"] != true || len(details["dependents"].([]string)) != 50 {
		t.Fatalf("preview details = %+v", details)
	}
	if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("dependency refusal had effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}

	h = newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return installedVersion("1.6", "1.6"), nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		switch {
		case strings.Contains(sql, "count(*)"):
			return []map[string]string{{"n": "51"}}, nil
		case strings.Contains(sql, "pg_describe_object"):
			rows := make([]map[string]string, 51)
			for i := range rows {
				rows[i] = map[string]string{"object": "object_" + string(rune('a'+i%26))}
			}
			return rows, nil
		default:
			return nil, nil
		}
	}
	env = &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app", "cascade": true}, "app")}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extRemoveResult)
	if !data.Cascade || data.DependentsTotal != 51 {
		t.Fatalf("result = %+v", data)
	}
	if len(h.pg.execs) != 1 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION IF EXISTS \"pg_trgm\" CASCADE") {
		t.Fatalf("drop execs = %v", h.pg.execs)
	}
	if len(h.saves) != 1 || len(h.applyCalls) != 1 {
		t.Fatalf("cascade effects: saves=%d apply=%d", len(h.saves), len(h.applyCalls))
	}
}

func TestExtRemoveSQLAbsentReconcilesIntent(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extRemoveResult)
	if !data.Enabled || data.Installed {
		t.Fatalf("result = %+v", data)
	}
	if len(h.pg.execs) != 0 || len(h.saves) != 1 || len(h.applyCalls) != 1 {
		t.Fatalf("reconcile effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}

func TestExtRemoveSaveFailureAfterDropIsPartial(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	h.saveErrAt[1] = errors.New("disk full")
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("save failure: %v", err)
	}
	details := extDetails(t, err)
	if details["sql_dropped"] != true || details["intent_saved"] != false {
		t.Fatalf("details = %+v", details)
	}
	if len(h.pg.execs) != 1 || len(h.applyCalls) != 0 {
		t.Fatalf("effects: exec=%v apply=%d", h.pg.execs, len(h.applyCalls))
	}
}

func TestExtRemoveApplyFailureAfterSaveIsPartial(t *testing.T) {
	h := newExtHarness(t, extTestCfg("pg_stat_statements", "pg_trgm"))
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	h.applyFn = func(engine.Options) (engine.ApplyData, error) {
		return engine.ApplyData{}, errors.New("restart failed")
	}
	env := &Env{Cluster: h.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_apply_failed" {
		t.Fatalf("apply failure: %v", err)
	}
	details := extDetails(t, err)
	if details["sql_dropped"] != true || details["intent_saved"] != true || !strings.Contains(details["retry"].(string), "vaire apply --db app") {
		t.Fatalf("details = %+v", details)
	}
	if len(h.saves) != 1 || len(h.applyCalls) != 1 {
		t.Fatalf("effects: saves=%d apply=%d", len(h.saves), len(h.applyCalls))
	}
}

func TestExtensionMutationRefusals(t *testing.T) {
	cfg := extTestCfg()
	follower := config.ClusterSpec{Name: "app_follower", Node: "db1", Port: 5434, Stanza: "follower-abc123", Size: "small", StatementTimeout: "60s", Follows: "app"}
	cases := []struct {
		name   string
		cfg    config.Cluster
		recs   backup.Records
		spec   config.ClusterSpec
		reason string
	}{
		{"follower", cfg, backup.Records{}, follower, "is_follower"},
		{"awaiting", cfg, backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}, cfg.Clusters[0], "awaiting_recovery"},
		{"open restore", cfg, backup.Records{Restores: map[string]backup.Restore{"app": {ID: "r1", Phase: "ready"}}}, cfg.Clusters[0], "restore_in_progress"},
		{"unfollow", cfg, backup.Records{Unfollowing: map[string]backup.Unfollow{"app": {Source: "other"}}}, cfg.Clusters[0], "unfollow_in_progress"},
		{"source of unfollow", cfg, backup.Records{Unfollowing: map[string]backup.Unfollow{"app_follower": {Source: "app"}}}, cfg.Clusters[0], "unfollow_in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := extensionMutationRefusals(tc.cfg, tc.recs, tc.spec, "an extension change")
			if err == nil || extReason(t, err) != tc.reason {
				t.Fatalf("refusal = %v, want %s", err, tc.reason)
			}
		})
	}

	withFollower := cfg.Clone()
	withFollower.Clusters = append(withFollower.Clusters, follower)
	err := extensionMutationRefusals(withFollower, backup.Records{}, withFollower.Clusters[0], "an extension change")
	if err == nil || extReason(t, err) != "follower_not_running" {
		t.Fatalf("source with follower = %v", err)
	}
}

func TestExtensionConfirmTokensAreTargetOnly(t *testing.T) {
	cfg := extTestCfg()
	newExtHarness(t, cfg)
	if got := extensionAddTokens(cfg, json.RawMessage(`{"db":"app"}`)); !slices.Equal(got, []string{"app/restart"}) {
		t.Fatalf("add tokens = %v", got)
	}
	if got := extensionAddTokens(cfg, json.RawMessage(`{"name":"vector"}`)); !slices.Equal(got, []string{"app/restart"}) {
		t.Fatalf("inferred add tokens = %v", got)
	}
	if got := extensionRemoveTokens(cfg, json.RawMessage(`{"db":"app"}`)); !slices.Equal(got, []string{"app", "app/restart"}) {
		t.Fatalf("remove tokens = %v", got)
	}
	two := cfg.Clone()
	two.Clusters = append(two.Clusters, config.ClusterSpec{Name: "other", Port: 5434, Size: "small", StatementTimeout: "60s"})
	if got := extensionAddTokens(two, json.RawMessage(`{"name":"vector"}`)); got != nil {
		t.Fatalf("two-database inference accepted: %v", got)
	}
}

func TestExtListFactsAndUnmanaged(t *testing.T) {
	dir := t.TempDir()
	oldDir := extensionControlDir
	extensionControlDir = dir
	t.Cleanup(func() { extensionControlDir = oldDir })
	for name, version := range map[string]string{"vector": "0.9.0", "pg_stat_statements": "1.12"} {
		if err := os.WriteFile(filepath.Join(dir, name+".control"), []byte("default_version = '"+version+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := extTestCfg("pg_stat_statements", "vector")
	stopped := config.ClusterSpec{Name: "stopped", Node: "db1", Port: 5434, Stanza: "stopped-abc123", Size: "small", StatementTimeout: "60s"}
	awaiting := config.ClusterSpec{Name: "old", Node: "db1", Port: 5435, Stanza: "old-abc123", Size: "small", StatementTimeout: "60s"}
	cfg.Clusters = append(cfg.Clusters, stopped, awaiting)
	h := newExtHarness(t, cfg)
	h.records = backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"old": {}}}
	h.runningFn = func(spec config.ClusterSpec) bool { return spec.Name != "stopped" }
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"},
			"vector":             {Name: "vector", Version: "0.8.0", Default: "0.9.0"},
			"plpgsql":            {Name: "plpgsql", Version: "1.0", Default: "1.0"},
			"mystery":            {Name: "mystery", Version: "2.0", Default: "2.0"},
		}, nil
	}
	env := &Env{Cluster: cfg}
	res, err := handleExtList(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extListData)
	if len(data.Databases) != 3 {
		t.Fatalf("databases = %+v", data.Databases)
	}
	byDB := map[string]extListDatabase{}
	for _, db := range data.Databases {
		byDB[db.DB] = db
	}
	if !byDB["app"].Available || byDB["stopped"].Available || byDB["old"].Available {
		t.Fatalf("availability = app:%v stopped:%v old:%v", byDB["app"].Available, byDB["stopped"].Available, byDB["old"].Available)
	}
	if byDB["stopped"].State != stateStopped || byDB["old"].State != stateAwaiting {
		t.Fatalf("states = %+v", byDB)
	}
	if len(byDB["app"].Unmanaged) != 1 || byDB["app"].Unmanaged[0].Name != "mystery" {
		t.Fatalf("unmanaged = %+v", byDB["app"].Unmanaged)
	}
	var vector *extListEntry
	for i := range data.Extensions {
		if data.Extensions[i].Name == "vector" {
			vector = &data.Extensions[i]
		}
	}
	if vector == nil || vector.DefaultVersion != "0.9.0" || !vector.Available {
		t.Fatalf("vector entry = %+v", vector)
	}
	if len(vector.Databases) != 1 {
		t.Fatalf("vector databases = %+v", vector.Databases)
	}
	st := vector.Databases[0]
	if st.DB != "app" || !st.Installed || st.Version != "0.8.0" || !st.UpdateAvailable {
		t.Fatalf("vector app state = %+v", st)
	}
}

func TestParseExtensionControlDefaultVersion(t *testing.T) {
	if got := parseExtensionControlDefaultVersion("comment = 'x'\ndefault_version = '0.9.0' # package default\n"); got != "0.9.0" {
		t.Fatalf("default version = %q", got)
	}
	if got := parseExtensionControlDefaultVersion("module_pathname = '$libdir/x'\n"); got != "" {
		t.Fatalf("missing default version = %q", got)
	}
}

func TestExtensionVersionCompare(t *testing.T) {
	if extensionUpdateAvailable("2.0", "1.0") {
		t.Fatal("an older default was reported as an update")
	}
	if !extensionUpdateAvailable("1.0", "2.0") {
		t.Fatal("a newer default was not reported as an update")
	}
	if extensionUpdateAvailable("1.0", "") {
		t.Fatal("an unknown default was reported as an update")
	}
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.2", "1.10", -1, true},
		{"2.0", "1.9", 1, true},
		{"1.0.0", "1.0", 0, true},
		{"1.0-beta", "1.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := extensionVersionCompare(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("compare(%q,%q) = %d,%v want %d,%v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

func TestExtActivityCarriesNoDescriptorOrSQLPayload(t *testing.T) {
	const canary = "EXT-CANARY-do-not-record"
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := rpc.Request{
		RequestID: testRequestID,
		Method:    "ext.add",
		Params:    json.RawMessage(`{"name":"custom","db":"app","settings":{"custom.mode":"` + canary + `"},"sql":"` + canary + `"}`),
	}
	scope := newRPCScope(rec, "ext.add", req)
	if err := scope.begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := scope.finish(context.Background(), activity.OutcomeSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, ev := range store.recorded() {
		if ev.Action != "ext.add" || ev.Database != "app" {
			t.Fatalf("typed metadata = %+v", ev)
		}
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), canary) {
			t.Fatalf("extension payload reached activity: %s", raw)
		}
	}
}

func TestActivityIncompleteAndExtensionClassification(t *testing.T) {
	outcome, code, reason := activityStepOutcome("incomplete", "")
	if outcome != "unknown" || code != "" || reason != "incomplete" {
		t.Fatalf("incomplete step = %q %q %q", outcome, code, reason)
	}
	table := methods()
	if !activityImportant(table["ext.add"], rpc.Request{}) || activityImportant(table["ext.add"], rpc.Request{DryRun: true}) {
		t.Fatal("ext.add activity classification is wrong")
	}
	if !activityImportant(table["ext.remove"], rpc.Request{}) || activityImportant(table["ext.remove"], rpc.Request{DryRun: true}) {
		t.Fatal("ext.remove activity classification is wrong")
	}
	if activityImportant(table["ext.list"], rpc.Request{}) {
		t.Fatal("ext.list is classified as a mutation")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertNoExtEffects(t *testing.T, h *extHarness) {
	t.Helper()
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("effects: saves=%d prepare=%d apply=%d exec=%d restart=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestExtAddSharedDefinitionChangeRefusedBeyondTarget(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	cfg.Clusters = append(cfg.Clusters, extAddCluster("other", 5434, "pg_stat_statements"))
	h := newExtHarness(t, cfg)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app",
		"settings": map[string]string{"pg_stat_statements.max": "20000"},
	}, "app/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_global_intent_conflict" {
		t.Fatalf("shared definition change: %v", err)
	}
	affected, _ := extDetails(t, err)["affected"].([]string)
	if !slices.Contains(affected, "other") {
		t.Fatalf("affected = %v", affected)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddSharedDefinitionRefusedForSourceFollowers(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	cfg.Clusters = append(cfg.Clusters,
		extAddCluster("source", 5434, "pg_stat_statements"),
		extFollowerCluster("source_follower", "source", 5435),
	)
	h := newExtHarness(t, cfg)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app",
		"settings": map[string]string{"pg_stat_statements.max": "20000"},
	}, "app/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_global_intent_conflict" {
		t.Fatalf("source follower definition change: %v", err)
	}
	affected, _ := extDetails(t, err)["affected"].([]string)
	if !slices.Contains(affected, "source") || !slices.Contains(affected, "source_follower") {
		t.Fatalf("affected = %v", affected)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddSharedDefinitionRefusedForRecoveryDeclaration(t *testing.T) {
	recovery := []string{"pg_trgm"}
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
		Name: "old", Node: "db1", Port: 5434, Stanza: "old-abc123", Size: "small",
		StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}, RecoveryExtensions: &recovery,
	})
	h := newExtHarness(t, cfg)
	h.records = backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"old": {}}}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_trgm", "db": "app",
		"settings": map[string]string{"pg_trgm.similarity_threshold": "1"},
	}, "app/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_global_intent_conflict" {
		t.Fatalf("recovery definition change: %v", err)
	}
	affected, _ := extDetails(t, err)["affected"].([]string)
	if !slices.Contains(affected, "old") {
		t.Fatalf("affected = %v", affected)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddSharedDefinitionAllowedWhenOnlyTargetUsesIt(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	cfg.Clusters = append(cfg.Clusters, extAddCluster("other", 5434, "pg_stat_statements"))
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("1.6", "1.6"), nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_trgm", "db": "app",
		"settings": map[string]string{"pg_trgm.similarity_threshold": "1"},
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if len(h.saves) != 1 {
		t.Fatalf("saves = %d", len(h.saves))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_trgm"].Settings["pg_trgm.similarity_threshold"] != "1" {
		t.Fatalf("saved definition = %+v", saved.Extensions["pg_trgm"])
	}
}

func TestExtAddOldJournalHashesDoNotNoOp(t *testing.T) {
	oldCfg := extTestCfg("pg_stat_statements")
	journal := filepath.Join(t.TempDir(), "journal.json")
	stepsByID := map[string]engine.Step{}
	for _, step := range engine.Expand(steps.Catalog(), oldCfg) {
		stepsByID[step.ID()] = step
	}
	j := &engine.Journal{Schema: engine.JournalSchema, Steps: map[string]engine.JournalEntry{}}
	for _, id := range []string{steps.ConfigStepID("app"), steps.RolesStepID("app")} {
		inputs, err := stepsByID[id].Inputs(oldCfg, engine.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		j.Steps[id] = engine.JournalEntry{Status: engine.StatusCompleted, InputsHash: engine.InputsHash(id, inputs)}
	}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	oldPath := extensionJournalPath
	extensionJournalPath = journal
	t.Cleanup(func() { extensionJournalPath = oldPath })

	copySafe, standby := "safe", true
	cfg := oldCfg.Clone()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {
			Source: catalog.SourceAPT, Package: "postgresql-18-pgstat-new",
			CopySafety: &copySafe, StandbySafe: &standby,
		},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	extensionJournalConverged = extensionJournalConvergedDefault
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_stat_statements", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if h.prepareCalls != 1 || len(h.applyCalls) != 2 || len(h.saves) != 0 {
		t.Fatalf("old journal hashes were treated as converged: prepare=%d apply=%d saves=%d", h.prepareCalls, len(h.applyCalls), len(h.saves))
	}
	prepared, err := h.prepareCfg.ExtensionResolver().Resolve("pg_stat_statements")
	if err != nil || prepared.Package != "postgresql-18-pgstat-new" || h.applyCfg.Extensions["pg_stat_statements"].Package != "postgresql-18-pgstat-new" {
		t.Fatalf("new provider was not processed: prepared=%+v apply=%+v err=%v", prepared, h.applyCfg.Extensions["pg_stat_statements"], err)
	}
	if data := res.Data.(extAddResult); data.Version != "1.12" {
		t.Fatalf("result = %+v", data)
	}
}

func TestExtAddProviderChangeRequiresFreshSafety(t *testing.T) {
	copySafe, standby := "safe", true
	base := extTestCfg()
	base.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {
			Source: catalog.SourceAPT, Package: "postgresql-18-pgstat-a",
			CopySafety: &copySafe, StandbySafe: &standby,
		},
	}
	cases := []struct {
		name    string
		params  map[string]any
		missing []string
	}{
		{"package change without safety", map[string]any{"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b"}, []string{"copy_safety", "standby_safe"}},
		{"package change missing standby", map[string]any{"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b", "copy_safety": "safe"}, []string{"standby_safe"}},
		{"package change missing copy", map[string]any{"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b", "standby_safe": true}, []string{"copy_safety"}},
		{"source change without safety", map[string]any{"name": "pg_stat_statements", "db": "app", "preinstalled": true}, []string{"copy_safety", "standby_safe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := base.Encode()
			if err != nil {
				t.Fatal(err)
			}
			h := newExtHarness(t, base)
			env := &Env{Cluster: base, Req: extReq(t, tc.params, "app/restart")}
			_, err = handleExtAdd(context.Background(), env)
			if err == nil || extReason(t, err) != "extension_safety_renewal_required" {
				t.Fatalf("provider change: %v", err)
			}
			missing, _ := extDetails(t, err)["missing"].([]string)
			if !slices.Equal(missing, tc.missing) {
				t.Fatalf("missing = %v, want %v", missing, tc.missing)
			}
			after, err := h.cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("refused provider change altered cluster.toml")
			}
			assertNoExtEffects(t, h)
		})
	}

	// A provider change on a new built-in override follows the same rule.
	noOverride := extTestCfg()
	before, err := noOverride.Encode()
	if err != nil {
		t.Fatal(err)
	}
	h := newExtHarness(t, noOverride)
	env := &Env{Cluster: noOverride, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b",
	}, "app/restart")}
	_, err = handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_safety_renewal_required" {
		t.Fatalf("new override provider change: %v", err)
	}
	after, err := h.cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refused new override altered cluster.toml")
	}
	assertNoExtEffects(t, h)

	// Both renewed declarations accept a target-only provider change.
	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env = &Env{Cluster: base, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b",
		"copy_safety": "safe", "standby_safe": true,
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_stat_statements"].Package != "postgresql-18-pgstat-b" ||
		saved.Extensions["pg_stat_statements"].CopySafety == nil || *saved.Extensions["pg_stat_statements"].CopySafety != "safe" ||
		saved.Extensions["pg_stat_statements"].StandbySafe == nil || !*saved.Extensions["pg_stat_statements"].StandbySafe {
		t.Fatalf("saved provider change = %+v", saved.Extensions["pg_stat_statements"])
	}

	// Explicit --standby-safe=false is a supplied declaration.
	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env = &Env{Cluster: base, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app", "package": "postgresql-18-pgstat-b",
		"copy_safety": "safe", "standby_safe": false,
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, err = config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_stat_statements"].StandbySafe == nil || *saved.Extensions["pg_stat_statements"].StandbySafe {
		t.Fatalf("explicit standby false lost: %+v", saved.Extensions["pg_stat_statements"])
	}

	// A preload provider change follows the same renewal rule.
	copyDisabled := "disabled"
	preloadBase := extTestCfg("pg_stat_statements", "pg_cron")
	preloadBase.Extensions = map[string]config.ExtensionDefinition{
		"pg_cron": {Source: catalog.SourceAPT, Package: "postgresql-18-cron", Preload: "one", CopySafety: &copyDisabled, StandbySafe: &standby},
	}
	h = newExtHarness(t, preloadBase)
	env = &Env{Cluster: preloadBase, Req: extReq(t, map[string]any{
		"name": "pg_cron", "db": "app", "preload": "two",
	}, "app/restart")}
	_, err = handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_safety_renewal_required" {
		t.Fatalf("preload change: %v", err)
	}
	assertNoExtEffects(t, h)

	h = newExtHarness(t, preloadBase)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	env = &Env{Cluster: preloadBase, Req: extReq(t, map[string]any{
		"name": "pg_cron", "db": "app", "preload": "two", "copy_safety": "disabled", "standby_safe": true,
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, err = config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_cron"].Preload != "two" {
		t.Fatalf("renewed preload change = %+v", saved.Extensions["pg_cron"])
	}

	// Source renewal is accepted when both declarations are explicit.
	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env = &Env{Cluster: base, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app", "preinstalled": true,
		"copy_safety": "safe", "standby_safe": true,
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, err = config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_stat_statements"].Source != catalog.SourcePreinstalled || saved.Extensions["pg_stat_statements"].Package != "" {
		t.Fatalf("renewed source change = %+v", saved.Extensions["pg_stat_statements"])
	}

	// Existing custom definitions still refuse direct replacement rather than
	// inheriting stale safety.
	custom := extTestCfg()
	custom.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom-a", CopySafety: &copySafe, StandbySafe: &standby},
	}
	h = newExtHarness(t, custom)
	env = &Env{Cluster: custom, Req: extReq(t, map[string]any{
		"name": "custom", "db": "app", "package": "postgresql-18-custom-b",
	}, "app/restart")}
	_, err = handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_definition_exists" {
		t.Fatalf("existing custom replacement: %v", err)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddPartialDirectFlagsPreserveExistingOverride(t *testing.T) {
	copySafe := "safe"
	standby := true
	cfg := extTestCfg()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {
			Source: catalog.SourceAPT, Package: "postgresql-18-pgstat-custom",
			Schema: "public", Settings: map[string]string{"pg_stat_statements.track": "top"},
			CopySafety: &copySafe, StandbySafe: &standby,
		},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.12", Default: "1.12"}}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_stat_statements", "db": "app",
		"settings": map[string]string{"pg_stat_statements.max": "20000"},
	}, "app/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.saves) != 1 {
		t.Fatalf("saves = %d", len(h.saves))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	def := saved.Extensions["pg_stat_statements"]
	if def.Source != catalog.SourceAPT || def.Package != "postgresql-18-pgstat-custom" ||
		def.Schema != "public" || def.CopySafety == nil || *def.CopySafety != "safe" ||
		def.StandbySafe == nil || !*def.StandbySafe {
		t.Fatalf("provider/safety/schema lost: %+v", def)
	}
	if def.Settings["pg_stat_statements.track"] != "top" || def.Settings["pg_stat_statements.max"] != "20000" {
		t.Fatalf("settings lost: %+v", def.Settings)
	}
}

func TestExtAddPartialFlagsPreservePreloadAndCopySettings(t *testing.T) {
	copySafe := "disabled"
	standby := true
	cfg := extTestCfg("pg_stat_statements", "vector")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"vector": {
			Source: catalog.SourcePreinstalled, Preload: "vector_alt", Schema: "public",
			Settings:   map[string]string{"vector.foo": "on"},
			CopySafety: &copySafe, CopySettings: map[string]string{"vector.copy_mode": "off"}, StandbySafe: &standby,
		},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"vector": {Name: "vector", Version: "0.9.0", Default: "0.9.0"}}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "vector", "db": "app", "settings": map[string]string{"vector.bar": "on"},
	}, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	def := saved.Extensions["vector"]
	if def.Source != catalog.SourcePreinstalled || def.Preload != "vector_alt" || def.Schema != "public" ||
		def.CopySafety == nil || *def.CopySafety != "disabled" || def.CopySettings["vector.copy_mode"] != "off" ||
		def.StandbySafe == nil || !*def.StandbySafe {
		t.Fatalf("preload/copy/safety lost: %+v", def)
	}
	if def.Settings["vector.foo"] != "on" || def.Settings["vector.bar"] != "on" {
		t.Fatalf("settings lost: %+v", def.Settings)
	}
}

func TestExtAddLoadedExtensionPendingSettingRequiresRestartToken(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_cron")
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "pg_cron": true}
	h.pendingSettings = []string{"cron.max_running_jobs"}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	params := map[string]any{"name": "pg_cron", "db": "app"}
	env := &Env{Cluster: cfg, Req: extReq(t, params)}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app/restart" {
		t.Fatalf("pending setting without token: %v", err)
	}
	assertNoExtEffects(t, h)

	env = &Env{Cluster: cfg, Req: extReq(t, params, "app/restart")}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.Restarted || !data.PendingRestart {
		t.Fatalf("result = %+v", data)
	}
	if h.restartCalls != 1 || len(h.applyCalls) != 2 || len(h.saves) != 0 {
		t.Fatalf("effects: restart=%d apply=%d saves=%d", h.restartCalls, len(h.applyCalls), len(h.saves))
	}
}

func TestExtAddConfigDriftRequiresRestartToken(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("1.6", "1.6"), nil
	}
	h.checkStepFn = func(id string) (string, error) {
		if id == steps.ConfigStepID("app") {
			return "the rendered configuration differs", nil
		}
		return "", nil
	}
	params := map[string]any{"name": "pg_trgm", "db": "app"}
	env := &Env{Cluster: cfg, Req: extReq(t, params)}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app/restart" {
		t.Fatalf("config drift without token: %v", err)
	}
	assertNoExtEffects(t, h)

	env = &Env{Cluster: cfg, Req: extReq(t, params, "app/restart")}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if h.prepareCalls != 1 || len(h.applyCalls) != 2 {
		t.Fatalf("config drift did not converge: prepare=%d apply=%d", h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtAddStableSameIntentIsIdempotent(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_cron")
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "pg_cron": true}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if !data.AlreadyEnabled || data.RestartRequired || data.PendingRestart {
		t.Fatalf("result = %+v", data)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddStaleJournalDoesNotNoOp(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_cron")
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "pg_cron": true}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	h.journalConverged = false
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_cron", "db": "app"})}
	if _, err := handleExtAdd(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if h.prepareCalls != 1 || len(h.applyCalls) != 2 {
		t.Fatalf("stale journal was treated as a no-op: prepare=%d apply=%d", h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtAddRetryAfterConfigFailureFinishesSavedIntent(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if len(opts.Only) == 1 && opts.Only[0] == steps.ConfigStepID("app") {
			return engine.ApplyData{}, errors.New("config write failed")
		}
		return engine.ApplyData{}, nil
	}
	params := map[string]any{"name": "pg_cron", "db": "app"}
	env := &Env{Cluster: cfg, Req: extReq(t, params, "app/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_apply_failed" {
		t.Fatalf("first apply: %v", err)
	}
	if len(h.saves) != 1 || len(h.applyCalls) != 1 {
		t.Fatalf("first effects: saves=%d apply=%d", len(h.saves), len(h.applyCalls))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	h.cfg, h.raw = saved, h.saves[0]
	h.saves, h.applyCalls, h.applyFn = nil, nil, nil
	h.installedCalls = 0
	h.pendingSettings = []string{"shared_preload_libraries"}

	env = &Env{Cluster: h.cfg, Req: extReq(t, params, "app/restart")}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extAddResult)
	if data.Version != "1.6" || !data.Restarted {
		t.Fatalf("result = %+v", data)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 2 || h.restartCalls != 1 {
		t.Fatalf("retry effects: saves=%d apply=%d restart=%d", len(h.saves), len(h.applyCalls), h.restartCalls)
	}
}

func TestExtAddNonnumericUpdateDirectionRefused(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return installedVersion("v2", "v1"), nil
	}
	h.updatePath = true
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_update_direction_unknown" {
		t.Fatalf("nonnumeric update: %v", err)
	}
	if h.updatePathCalls != 0 || len(h.pg.execs) != 0 || len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("unknown direction had effects: path=%d exec=%v saves=%d prepare=%d apply=%d", h.updatePathCalls, h.pg.execs, len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtRemoveSettingsRemovalRequiresRestartTokenBeforeEffects(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_trgm": {Settings: map[string]string{"max_files_per_process": "2000"}},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	params := map[string]any{"name": "pg_trgm", "db": "app"}
	env := &Env{Cluster: cfg, Req: extReq(t, params, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app/restart" {
		t.Fatalf("settings removal without restart token: %v", err)
	}
	if h.restartCalls != 0 {
		t.Fatalf("missing restart token restarted %d times", h.restartCalls)
	}
	assertNoExtEffects(t, h)

	h = newExtHarness(t, cfg)
	h.pendingSettings = []string{"max_files_per_process"}
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return installedVersion("1.6", "1.6"), nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	env = &Env{Cluster: cfg, Req: extReq(t, params, "app", "app/restart")}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extRemoveResult)
	if !data.RestartRequired || !data.Restarted {
		t.Fatalf("result = %+v", data)
	}
	if len(h.pg.execs) != 1 || len(h.saves) != 1 || len(h.applyCalls) != 1 || h.restartCalls != 1 {
		t.Fatalf("effects: exec=%v saves=%d apply=%d restart=%d", h.pg.execs, len(h.saves), len(h.applyCalls), h.restartCalls)
	}
}

func TestExtRemoveDryRunHasZeroEffects(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_trgm": {Settings: map[string]string{"max_files_per_process": "2000"}},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	env := &Env{Cluster: cfg, Req: rpc.Request{Params: mustJSON(t, map[string]any{"name": "pg_trgm", "db": "app"}), DryRun: true}}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extRemoveResult)
	if !data.RestartRequired || !slices.Contains(data.RequiredConfirmTokens, "app") || !slices.Contains(data.RequiredConfirmTokens, "app/restart") {
		t.Fatalf("result = %+v", data)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddCustomUnauthorizedHasZeroEffects(t *testing.T) {
	cfg := extTestCfg()
	h := newExtHarness(t, cfg)
	standby := true
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "custom_ext", "db": "app", "package": "postgresql-18-custom",
		"preload": "custom_lib", "settings": map[string]string{"custom.mode": "on"},
		"copy_safety": "safe", "standby_safe": standby,
	})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extDetails(t, err)["confirm_token"] != "app/restart" {
		t.Fatalf("custom without token: %v", err)
	}
	assertNoExtEffects(t, h)
}

func TestExtHandlerRefusalsHaveZeroEffects(t *testing.T) {
	base := extTestCfg()
	follower := base.Clone()
	follower.Clusters[0].Follows = "other"
	cases := []struct {
		name string
		cfg  config.Cluster
		recs backup.Records
	}{
		{"follower", follower, backup.Records{}},
		{"awaiting", base, backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}},
		{"open restore", base, backup.Records{Restores: map[string]backup.Restore{"app": {ID: "r1", Phase: "ready"}}}},
		{"unfollow", base, backup.Records{Unfollowing: map[string]backup.Unfollow{"app": {Source: "other"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newExtHarness(t, tc.cfg)
			h.records = tc.recs
			addEnv := &Env{Cluster: tc.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
			if _, err := handleExtAdd(context.Background(), addEnv); err == nil {
				t.Fatal("add was accepted")
			}
			removeEnv := &Env{Cluster: tc.cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"}, "app")}
			if _, err := handleExtRemove(context.Background(), removeEnv); err == nil {
				t.Fatal("remove was accepted")
			}
			assertNoExtEffects(t, h)
		})
	}
}

func TestExtListOverrideShowsProviderAndActualVersion(t *testing.T) {
	controlDir, libDir := t.TempDir(), t.TempDir()
	oldControl, oldLib := extensionControlDir, extensionLibDir
	extensionControlDir, extensionLibDir = controlDir, libDir
	t.Cleanup(func() { extensionControlDir, extensionLibDir = oldControl, oldLib })
	if err := os.WriteFile(filepath.Join(controlDir, "vector.control"), []byte("default_version = '0.9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "vector_alt.so"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	copySafe := "safe"
	standby := true
	cfg := extTestCfg("pg_stat_statements", "vector")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"vector": {Source: catalog.SourcePreinstalled, Preload: "vector_alt", Schema: "public", CopySafety: &copySafe, StandbySafe: &standby},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"vector": {Name: "vector", Version: "0.8.0", Default: "0.9.0"}}, nil
	}
	res, err := handleExtList(context.Background(), &Env{Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extListData)
	var vector *extListEntry
	for i := range data.Extensions {
		if data.Extensions[i].Name == "vector" {
			vector = &data.Extensions[i]
		}
	}
	if vector == nil || vector.Source != catalog.SourcePreinstalled || vector.Package != "" || vector.Preload != "vector_alt" {
		t.Fatalf("vector override = %+v", vector)
	}
	if len(vector.Databases) != 1 || vector.Databases[0].Version != "0.8.0" || vector.Databases[0].DefaultVersion != "0.9.0" {
		t.Fatalf("vector actual facts = %+v", vector.Databases)
	}
}

func TestExtRemovePackageKept(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "vector")
	h := newExtHarness(t, cfg)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{"vector": {Name: "vector", Version: "0.9.0", Default: "0.9.0"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "vector", "db": "app"}, "app")}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Data.(extRemoveResult).PackageKept; got != "postgresql-18-pgvector" {
		t.Fatalf("package kept = %q", got)
	}
}

func TestExtRemoveSQLAbsentRetryAfterSaveFailure(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements", "pg_trgm")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) { return installedVersion("1.6", "1.6"), nil }
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "count(*)") {
			return []map[string]string{{"n": "0"}}, nil
		}
		return nil, nil
	}
	h.saveErrAt[1] = errors.New("disk full")
	params := map[string]any{"name": "pg_trgm", "db": "app"}
	env := &Env{Cluster: cfg, Req: extReq(t, params, "app")}
	_, err := handleExtRemove(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_intent_save_failed" {
		t.Fatalf("first remove: %v", err)
	}
	if len(h.pg.execs) != 1 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("first effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}

	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.saveErrAt = map[int]error{}
	env = &Env{Cluster: cfg, Req: extReq(t, params, "app")}
	res, err := handleExtRemove(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(extRemoveResult)
	if !data.Enabled || data.Installed {
		t.Fatalf("retry result = %+v", data)
	}
	if len(h.pg.execs) != 1 || len(h.saves) != 1 || len(h.applyCalls) != 1 {
		t.Fatalf("retry effects: exec=%v saves=%d apply=%d", h.pg.execs, len(h.saves), len(h.applyCalls))
	}
}
