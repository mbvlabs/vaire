package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

// cronFake models the owned copy's cron metadata for settlement tests.
type cronFake struct {
	metadataDB  string
	metadataOID string
	currentName string
	present     bool
	columns     map[string]bool
	jobs        []map[string]string
	execs       []string
}

func (c *cronFake) Query(_ context.Context, db, sql string, vars map[string]string) ([]map[string]string, error) {
	switch {
	case strings.Contains(sql, "current_setting('cron.database_name')"):
		return []map[string]string{{"db": c.metadataDB, "oid": c.metadataOID}}, nil
	case strings.Contains(sql, "pg_database WHERE oid"):
		if vars["oid"] != c.metadataOID || c.metadataOID == "" {
			return nil, nil
		}
		return []map[string]string{{"db": c.currentName}}, nil
	case strings.Contains(sql, "to_regclass('cron.job')"):
		return []map[string]string{{"present": boolText(c.present)}}, nil
	case strings.Contains(sql, "information_schema.columns"):
		var rows []map[string]string
		for name := range c.columns {
			rows = append(rows, map[string]string{"name": name})
		}
		return rows, nil
	case strings.Contains(sql, "UPDATE cron.job SET active = false"):
		var rows []map[string]string
		for _, job := range c.jobs {
			if job["active"] == "true" {
				job["active"] = "false"
				rows = append(rows, map[string]string{"jobid": job["jobid"]})
			}
		}
		return rows, nil
	case strings.Contains(sql, "count(*)") && strings.Contains(sql, "WHERE active"):
		n := 0
		for _, job := range c.jobs {
			if job["active"] == "true" {
				n++
			}
		}
		return []map[string]string{{"n": cronItoa(n)}}, nil
	case strings.Contains(sql, "UPDATE cron.job SET database"):
		var rows []map[string]string
		for _, job := range c.jobs {
			if job["database"] == vars["old"] {
				job["database"] = vars["new"]
				rows = append(rows, map[string]string{"jobid": job["jobid"]})
			}
		}
		return rows, nil
	case strings.Contains(sql, "count(*)") && strings.Contains(sql, "WHERE database"):
		n := 0
		for _, job := range c.jobs {
			if job["database"] == vars["old"] {
				n++
			}
		}
		return []map[string]string{{"n": cronItoa(n)}}, nil
	}
	return nil, nil
}

func (c *cronFake) Exec(_ context.Context, _, sql string, _ map[string]string) error {
	c.execs = append(c.execs, sql)
	return nil
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func cronItoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func TestCronSettlementUsesActualMetadataDatabase(t *testing.T) {
	fake := &cronFake{metadataDB: "cron_db", metadataOID: "42", currentName: "cron_db", present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{
			{"jobid": "1", "active": "true", "database": "app"},
			{"jobid": "2", "active": "true", "database": "other"},
			{"jobid": "3", "active": "false", "database": "app"},
		}}
	s, err := deactivateCopiedCronJobs(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if s.MetadataDB != "cron_db" || s.Deactivated != 2 {
		t.Fatalf("settlement = %+v", s)
	}
	n, err := retargetCopiedCronJobs(context.Background(), fake, s, "app", "fork")
	if err != nil || n != 2 {
		t.Fatalf("retarget = %d, %v", n, err)
	}
	for _, job := range fake.jobs {
		if job["database"] == "app" {
			t.Fatalf("a job still names the old database: %+v", fake.jobs)
		}
		if job["jobid"] == "2" && job["database"] != "other" {
			t.Fatalf("an unrelated job was retargeted: %+v", job)
		}
	}
	if err := checkCronMetadataSetting(context.Background(), fake, "app", "fork"); err != nil {
		t.Fatalf("a different metadata database was refused: %v", err)
	}
}

func TestCronUnknownMetadataFailsNotZero(t *testing.T) {
	fake := &cronFake{metadataDB: "cron_db", present: false, columns: map[string]bool{"active": true, "database": true}}
	if _, err := deactivateCopiedCronJobs(context.Background(), fake); err == nil {
		t.Fatal("a missing cron.job table reported zero")
	}
	fake = &cronFake{metadataDB: "", present: true, columns: map[string]bool{"active": true, "database": true}}
	if _, err := deactivateCopiedCronJobs(context.Background(), fake); err == nil {
		t.Fatal("an empty metadata database was guessed")
	}
}

func TestCronStaleMetadataSettingRefused(t *testing.T) {
	fake := &cronFake{metadataDB: "app", present: true, columns: map[string]bool{"active": true, "database": true}}
	if err := checkCronMetadataSetting(context.Background(), fake, "app", "fork"); err == nil {
		t.Fatal("a metadata database left naming the renamed old database was accepted")
	}
}

func TestVerifyCopyGuardEffectiveAndPending(t *testing.T) {
	oldWrite := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error { return os.WriteFile(path, data, perm) })
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldWrite) })
	dir := t.TempDir()
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"pg_cron"}, Settings: map[string]string{"cron.launch_active_jobs": "off"},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &guardFake{loaded: "pg_stat_statements,pg_cron", setting: "off"}
	pending, err := verifyCopyGuard(context.Background(), fake, dir)
	if err != nil || len(pending) != 0 {
		t.Fatalf("effective guard = %v, %v", pending, err)
	}
	fake.setting = "on"
	fake.pending = true
	pending, err = verifyCopyGuard(context.Background(), fake, dir)
	if err != nil || len(pending) != 1 || pending[0] != "cron.launch_active_jobs" {
		t.Fatalf("pending guard = %v, %v", pending, err)
	}
	fake.pending = false
	if _, err := verifyCopyGuard(context.Background(), fake, dir); err == nil {
		t.Fatal("an ineffective setting with no pending restart was accepted")
	}
}

type guardFake struct {
	loaded         string
	setting        string
	pending        bool
	pendingPreload bool
}

func (g *guardFake) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	if strings.Contains(sql, "shared_preload_libraries") {
		if g.pendingPreload {
			return []map[string]string{{"v": "", "p": "true"}}, nil
		}
		return []map[string]string{{"v": g.loaded, "p": "false"}}, nil
	}
	if g.pending {
		return []map[string]string{{"v": "", "p": "true"}}, nil
	}
	return []map[string]string{{"v": g.setting, "p": "false"}}, nil
}

func (g *guardFake) Exec(context.Context, string, string, map[string]string) error { return nil }

func TestPruneCopyGuardKeepsRetainedPreloadOnly(t *testing.T) {
	useTempRecords(t)
	useTestInstance(t)
	extRoot := t.TempDir()
	controlDir, libDir := filepath.Join(extRoot, "extension"), filepath.Join(extRoot, "lib")
	for _, dir := range []string{controlDir, libDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	restoreExt := steps.ExtensionDirsForTest(controlDir, libDir)
	t.Cleanup(restoreExt)
	for _, path := range []string{
		filepath.Join(controlDir, "pg_stat_statements.control"),
		filepath.Join(controlDir, "old_ext.control"),
		filepath.Join(libDir, "pg_stat_statements.so"),
		filepath.Join(libDir, "auto_explain.so"),
		filepath.Join(libDir, "old_lib.so"),
	} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldWrite := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error { return os.WriteFile(path, data, perm) })
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldWrite) })
	restoreConf := steps.PGConfRootForTest(t.TempDir())
	t.Cleanup(restoreConf)
	confDir := steps.PGConfDir("app")
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"),
		[]byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "50-vaire-settings.conf"),
		[]byte("old_ext.mode = off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"old_lib"}, Settings: map[string]string{"old_ext.mode": "off"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"pg_trgm"},
		Settings: map[string]string{"old_ext.mode": "off"}}
	plan := &backup.CopyPlan{Requirements: []catalog.Extension{{
		Name: "old_ext", ExtName: "old_ext", Source: catalog.SourcePreinstalled,
		Preload: "old_lib", CopySafety: catalog.CopySafetyDisabled,
		CopySettings: map[string]string{"old_ext.mode": "off"}, StandbySafe: true,
	}}}
	oldRunning := clusterRunning
	clusterRunning = func(config.ClusterSpec) bool { return false }
	t.Cleanup(func() { clusterRunning = oldRunning })
	if _, err := pruneCopyGuardForSettlement(context.Background(), cfg, spec, dir, plan, true, "app", nil); err != nil {
		t.Fatal(err)
	}
	got, present, err := backup.ReadCopyGuard(dir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v", got, err)
	}
	if !contains(got.Preloads, "old_lib") || len(got.Settings) != 0 {
		t.Fatalf("independent copy guard = %+v", got)
	}
}

func TestReconcileDesiredSQLErrorsOnStandby(t *testing.T) {
	fake := &standbyFake{}
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}
	if _, err := reconcileDesiredExtensionSQL(context.Background(), cfg, spec, fake, "app"); err == nil {
		t.Fatal("desired SQL was created on a standby")
	}
	if len(fake.execs) != 0 {
		t.Fatalf("a standby ran SQL: %v", fake.execs)
	}
}

type standbyFake struct{ execs []string }

func (s *standbyFake) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	if strings.Contains(sql, "pg_is_in_recovery") {
		return []map[string]string{{"r": "true"}}, nil
	}
	return nil, nil
}
func (s *standbyFake) Exec(_ context.Context, _, sql string, _ map[string]string) error {
	s.execs = append(s.execs, sql)
	return nil
}

func TestCheckRestoredExtensionsBlocksUnknownAndReportsRetained(t *testing.T) {
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}
	fake := &restoredFake{installed: []string{"plpgsql", "pg_stat_statements", "old_ext", "unknown_ext"}}
	plan := &backup.CopyPlan{Requirements: []catalog.Extension{{
		Name: "old_ext", ExtName: "old_ext", Source: catalog.SourcePreinstalled,
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}}
	warnings, err := checkRestoredExtensions(context.Background(), cfg, spec, plan, fake, "app")
	if err == nil || !strings.Contains(err.Error(), "unknown_ext") {
		t.Fatalf("unknown restored extension = %v", err)
	}
	if len(warnings) == 0 || warnings[0].Code != "extension_retained" {
		t.Fatalf("retained warning = %+v", warnings)
	}
}

type restoredFake struct{ installed []string }

func (r *restoredFake) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	var rows []map[string]string
	for _, name := range r.installed {
		rows = append(rows, map[string]string{"name": name})
	}
	return rows, nil
}
func (r *restoredFake) Exec(context.Context, string, string, map[string]string) error { return nil }

func TestSettleRestoredCopyErrorKeepsCallerFromResuming(t *testing.T) {
	oldReconcile := reconcileCopySQL
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, errors.New("desired SQL failed")
	}
	t.Cleanup(func() { reconcileCopySQL = oldReconcile })
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}
	if _, err := settleRestoredCopy(context.Background(), cfg, spec, &restoredFake{}, t.TempDir(), &backup.CopyPlan{}, "app", nil); err == nil {
		t.Fatal("a desired SQL failure was reported as success")
	}
}

func TestGuardWriterTestHelpers(t *testing.T) {
	old := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(old)
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.auto.conf")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCopyGuardUsesOwnedEndpointAndRestartsOnPending(t *testing.T) {
	useTempGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldRunning, oldLocal := clusterRunning, copyGuardLocal
	clusterRunning = func(config.ClusterSpec) bool { return true }
	fake := &guardFake{setting: "on", pending: true}
	var gotPort int
	copyGuardLocal = func(spec config.ClusterSpec) sqlRunner { gotPort = spec.Port; return fake }
	t.Cleanup(func() { clusterRunning, copyGuardLocal = oldRunning, oldLocal })
	plan := &extensionCopyPlan{Plan: &backup.CopyPlan{
		Guard: backup.CopyGuard{Settings: map[string]string{"worker.enabled": "off"}},
	}}
	restarts := 0
	restart := func() error {
		restarts++
		fake.pending = false
		fake.setting = "off"
		return nil
	}
	if _, err := ensureCopyGuard(context.Background(), dataDir, plan,
		copyGuardEndpoint{Name: "app__restoring", Port: 5555, ConfDir: confDir}, restart); err != nil {
		t.Fatal(err)
	}
	if gotPort != 5555 || restarts != 1 {
		t.Fatalf("port=%d restarts=%d", gotPort, restarts)
	}
}

func TestEnsureCopyGuardStillPendingAfterRestartRefuses(t *testing.T) {
	useTempGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldRunning, oldLocal := clusterRunning, copyGuardLocal
	clusterRunning = func(config.ClusterSpec) bool { return true }
	copyGuardLocal = func(config.ClusterSpec) sqlRunner { return &guardFake{pendingPreload: true} }
	t.Cleanup(func() { clusterRunning, copyGuardLocal = oldRunning, oldLocal })
	plan := &extensionCopyPlan{Plan: &backup.CopyPlan{
		Guard: backup.CopyGuard{Preloads: []string{"retained_lib"}},
	}}
	if _, err := ensureCopyGuard(context.Background(), dataDir, plan,
		copyGuardEndpoint{Name: "app", Port: 5555, ConfDir: confDir}, func() error { return nil }); err == nil {
		t.Fatal("a guard still pending after restart was accepted")
	}
}

func TestCronMetadataIdentityResolvesRenameByOID(t *testing.T) {
	fake := &cronFake{metadataDB: "app", metadataOID: "42", currentName: "app_follower",
		present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	id, err := cronMetadataIdentity(context.Background(), fake)
	if err != nil || id.MetadataDB != "app" || id.MetadataOID != "42" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	current, err := cronMetadataCurrentName(context.Background(), fake, id)
	if err != nil || current != "app_follower" {
		t.Fatalf("current name = %q, %v", current, err)
	}
	settled, err := deactivateCopiedCronJobsAt(context.Background(), fake, &backup.UnfollowCron{MetadataDB: "app", MetadataOID: "42"})
	if err != nil || settled.MetadataDB != "app_follower" || settled.Deactivated != 1 {
		t.Fatalf("settlement = %+v, %v", settled, err)
	}
}

func useTempGuardWriter(t *testing.T) {
	t.Helper()
	old := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // G304: test-owned path
	})
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(old) })
}
