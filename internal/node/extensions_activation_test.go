package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

// cronFake models the owned copy's cron metadata for settlement tests.
type cronFake struct {
	metadataDB    string
	metadataOID   string
	currentName   string
	installed     bool
	extensionErr  error
	extensionRows []map[string]string
	installDB     string
	present       bool
	jobTableRows  []map[string]string
	columns       map[string]bool
	jobs          []map[string]string
	execs         []string
	jobReads      int
	jobDB         string
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
	case strings.Contains(sql, "FROM pg_extension"):
		c.installDB = db
		if c.extensionErr != nil {
			return nil, c.extensionErr
		}
		if c.extensionRows != nil {
			return c.extensionRows, nil
		}
		return []map[string]string{{"installed": boolText(c.installed)}}, nil
	case strings.Contains(sql, "to_regclass('cron.job')"):
		c.jobReads++
		c.jobDB = db
		if c.jobTableRows != nil {
			return c.jobTableRows, nil
		}
		// psql --csv emits t/f for native booleans, but true/false for text.
		value := "f"
		if c.present {
			value = "t"
		}
		if strings.Contains(sql, "::text AS present") {
			value = boolText(c.present)
		}
		return []map[string]string{{"present": value}}, nil
	case strings.Contains(sql, "information_schema.columns"):
		c.jobReads++
		c.jobDB = db
		var rows []map[string]string
		for name := range c.columns {
			rows = append(rows, map[string]string{"name": name})
		}
		return rows, nil
	case strings.Contains(sql, "UPDATE cron.job SET active = false"):
		c.jobReads++
		c.jobDB = db
		var rows []map[string]string
		for _, job := range c.jobs {
			if job["active"] == "true" {
				job["active"] = "false"
				rows = append(rows, map[string]string{"jobid": job["jobid"]})
			}
		}
		return rows, nil
	case strings.Contains(sql, "count(*)") && strings.Contains(sql, "WHERE active"):
		c.jobReads++
		c.jobDB = db
		n := 0
		for _, job := range c.jobs {
			if job["active"] == "true" {
				n++
			}
		}
		return []map[string]string{{"n": cronItoa(n)}}, nil
	case strings.Contains(sql, "UPDATE cron.job SET database"):
		c.jobReads++
		c.jobDB = db
		var rows []map[string]string
		for _, job := range c.jobs {
			if job["database"] == vars["old"] {
				job["database"] = vars["new"]
				rows = append(rows, map[string]string{"jobid": job["jobid"]})
			}
		}
		return rows, nil
	case strings.Contains(sql, "count(*)") && strings.Contains(sql, "WHERE database"):
		c.jobReads++
		c.jobDB = db
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

func TestCronJobTableUsesPSQLBooleanWireFormat(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(boolText(present), func(t *testing.T) {
			fake := &cronFake{present: present}
			got, err := cronJobTable(context.Background(), fake, "metadata")
			if got != present || (err != nil) == present {
				t.Fatalf("cron table present=%v: got %v, %v", present, got, err)
			}
			if fake.jobReads != 1 || fake.jobDB != "metadata" {
				t.Fatalf("queried wrong metadata: %+v", fake)
			}
		})
	}
}

func TestCronJobTableRefusesUnusableFacts(t *testing.T) {
	for _, rows := range [][]map[string]string{
		{},
		{{"present": "unknown"}},
		{{"present": "true"}, {"present": "true"}},
	} {
		if got, err := cronJobTable(context.Background(), &cronFake{jobTableRows: rows}, "metadata"); got || err == nil {
			t.Fatalf("accepted unusable cron table facts %+v: %v, %v", rows, got, err)
		}
	}
}

func TestCronSettlementUsesActualMetadataDatabase(t *testing.T) {
	fake := &cronFake{metadataDB: "cron_db", metadataOID: "42", currentName: "cron_db", installed: true, present: true, columns: map[string]bool{"active": true, "database": true},
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
	fake := &cronFake{metadataDB: "cron_db", metadataOID: "9", currentName: "cron_db", installed: true, present: false, columns: map[string]bool{"active": true, "database": true}}
	if _, err := deactivateCopiedCronJobs(context.Background(), fake); err == nil {
		t.Fatal("an installed pg_cron with no cron.job table reported zero")
	}
	fake = &cronFake{metadataDB: "", present: true, columns: map[string]bool{"active": true, "database": true}}
	if _, err := deactivateCopiedCronJobs(context.Background(), fake); err == nil {
		t.Fatal("an empty metadata database was guessed")
	}
}

func TestCronPresenceUnknownFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		fake   *cronFake
		reason string
	}{
		{
			name:   "query error",
			fake:   &cronFake{metadataDB: "cron_db", metadataOID: "9", currentName: "cron_db", extensionErr: errors.New("catalog unavailable")},
			reason: "unreadable pg_extension",
		},
		{
			name:   "no row",
			fake:   &cronFake{metadataDB: "cron_db", metadataOID: "9", currentName: "cron_db", extensionRows: []map[string]string{}},
			reason: "empty result",
		},
		{
			name:   "malformed value",
			fake:   &cronFake{metadataDB: "cron_db", metadataOID: "9", currentName: "cron_db", extensionRows: []map[string]string{{"installed": "maybe"}}},
			reason: "malformed result",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := deactivateCopiedCronJobs(context.Background(), tc.fake); err == nil {
				t.Fatalf("%s was treated as proven absence", tc.reason)
			}
			if tc.fake.jobReads != 0 {
				t.Fatalf("%s inspected cron.job", tc.reason)
			}
		})
	}
}

func TestCronAbsentExtensionSkipsJobsAndRetarget(t *testing.T) {
	fake := &cronFake{metadataDB: "postgres", metadataOID: "1", currentName: "postgres",
		present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	settled, err := deactivateCopiedCronJobs(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Installed || settled.Deactivated != 0 || settled.MetadataDB != "postgres" {
		t.Fatalf("an absent extension was settled as installed: %+v", settled)
	}
	if fake.jobReads != 0 {
		t.Fatalf("proven absence still inspected cron.job: %d reads", fake.jobReads)
	}
	if fake.jobs[0]["active"] != "true" {
		t.Fatalf("proven absence changed a job: %+v", fake.jobs)
	}
	n, err := retargetCopiedCronJobs(context.Background(), fake, settled, "app", "fork")
	if err != nil || n != 0 {
		t.Fatalf("retarget after absence = %d, %v", n, err)
	}
	if fake.jobReads != 0 || fake.jobs[0]["database"] != "app" {
		t.Fatalf("proven absence still retargeted jobs: %+v", fake.jobs)
	}
}

func TestCronSeparateMetadataDatabase(t *testing.T) {
	fake := &cronFake{metadataDB: "cron_db", metadataOID: "7", currentName: "cron_db", installed: true,
		present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	settled, err := deactivateCopiedCronJobs(context.Background(), fake)
	if err != nil || !settled.Installed || settled.Deactivated != 1 || settled.MetadataDB != "cron_db" {
		t.Fatalf("settlement = %+v, %v", settled, err)
	}
	if fake.installDB != "cron_db" {
		t.Fatalf("presence was read from %q, not the metadata database", fake.installDB)
	}
	n, err := retargetCopiedCronJobs(context.Background(), fake, settled, "app", "fork")
	if err != nil || n != 1 || fake.jobs[0]["database"] != "fork" {
		t.Fatalf("retarget = %d, %v, %+v", n, err, fake.jobs)
	}
}

func TestCronRenamedMetadataDatabase(t *testing.T) {
	fake := &cronFake{metadataDB: "app", metadataOID: "42", currentName: "app_follower", installed: true,
		present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	settled, err := deactivateCopiedCronJobsAt(context.Background(), fake, &backup.UnfollowCron{MetadataDB: "app", MetadataOID: "42"})
	if err != nil || !settled.Installed || settled.Deactivated != 1 || settled.MetadataDB != "app_follower" {
		t.Fatalf("settlement = %+v, %v", settled, err)
	}
	if fake.installDB != "app_follower" {
		t.Fatalf("presence was read from %q, not the renamed metadata database", fake.installDB)
	}
	n, err := retargetCopiedCronJobs(context.Background(), fake, settled, "app", "app_follower")
	if err != nil || n != 1 || fake.jobs[0]["database"] != "app_follower" {
		t.Fatalf("retarget = %d, %v, %+v", n, err, fake.jobs)
	}
}

func TestCronRetargetFollowsCopyRename(t *testing.T) {
	fake := &cronFake{metadataDB: "app", metadataOID: "42", currentName: "app", installed: true,
		present: true, columns: map[string]bool{"active": true, "database": true},
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	settled, err := deactivateCopiedCronJobs(context.Background(), fake)
	if err != nil || !settled.Installed || settled.MetadataDB != "app" {
		t.Fatalf("pre-rename settlement = %+v, %v", settled, err)
	}
	n, err := retargetCopiedCronJobs(context.Background(), fake, settled, "app", "fork")
	if err != nil || n != 1 || fake.jobs[0]["database"] != "fork" {
		t.Fatalf("retarget = %d, %v, %+v", n, err, fake.jobs)
	}
	if fake.jobDB != "fork" {
		t.Fatalf("retarget queried %q, not the renamed copy database", fake.jobDB)
	}
}

func TestCronAbsentInSeparateMetadataDatabase(t *testing.T) {
	fake := &cronFake{metadataDB: "cron_db", metadataOID: "7", currentName: "cron_db",
		jobs: []map[string]string{{"jobid": "1", "active": "true", "database": "app"}}}
	settled, err := deactivateCopiedCronJobs(context.Background(), fake)
	if err != nil || settled.Installed || settled.Deactivated != 0 {
		t.Fatalf("settlement = %+v, %v", settled, err)
	}
	if fake.installDB != "cron_db" || fake.jobReads != 0 {
		t.Fatalf("presence db=%q job reads=%d", fake.installDB, fake.jobReads)
	}
}

func TestCronStaleMetadataSettingRefused(t *testing.T) {
	fake := &cronFake{metadataDB: "app", present: true, columns: map[string]bool{"active": true, "database": true}}
	if err := checkCronMetadataSetting(context.Background(), fake, "app", "fork"); err == nil {
		t.Fatal("a metadata database left naming the renamed old database was accepted")
	}
}

// The data may not have pg_cron installed; the plan must still carry the
// historical prerequisite and its suppression. Deactivation's separate
// presence probe is covered below.
func TestRetainedCronKeepsPlanGuardWithoutInstalledExtension(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{cronRequirement(t)}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, Stanza: spec.Stanza, CopyDB: "app", IncludeRetained: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Plan.Cron || !hasMerged(plan.Merged, "pg_cron") {
		t.Fatalf("the retained cron requirement was dropped: %+v", plan.Merged)
	}
	if !contains(plan.Plan.Guard.Preloads, "pg_cron") {
		t.Fatalf("the retained cron library left the guard: %+v", plan.Plan.Guard)
	}
	if plan.Plan.Guard.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("the retained cron launch suppression left the guard: %+v", plan.Plan.Guard.Settings)
	}
	if plan.Plan.Guard.Settings["cron.database_name"] != "'app'" {
		t.Fatalf("the retained cron metadata setting was not resolved to the copy: %+v", plan.Plan.Guard.Settings)
	}
}

// Settlement must not drop a retained library and its launch suppression just
// because the application removed the SQL extension.
func TestPruneCopyGuardKeepsRetainedCronWithoutInstalledExtension(t *testing.T) {
	useTestInstance(t)
	useTempExtensionRecords(t)
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
		filepath.Join(controlDir, "pg_cron.control"),
		filepath.Join(controlDir, "pg_stat_statements.control"),
		filepath.Join(libDir, "pg_cron.so"),
		filepath.Join(libDir, "pg_stat_statements.so"),
		filepath.Join(libDir, "auto_explain.so"),
	} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	useTempGuardWriter(t)
	restoreConf := steps.PGConfRootForTest(t.TempDir())
	t.Cleanup(restoreConf)
	confDir := steps.PGConfDir("fork")
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"), []byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRunning := clusterRunning
	clusterRunning = func(config.ClusterSpec) bool { return false }
	t.Cleanup(func() { clusterRunning = oldRunning })
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "fork", Stanza: "fork-stanza"}
	dataDir := t.TempDir()
	plan := &backup.CopyPlan{Cron: true, Requirements: []catalog.Extension{cronRequirement(t)}}
	if _, err := pruneCopyGuardForSettlement(context.Background(), cfg, spec, dataDir, plan, true, "fork", nil); err != nil {
		t.Fatal(err)
	}
	got, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v", got, err)
	}
	if !contains(got.Preloads, "pg_cron") || got.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("an absent installed extension dropped the retained suppression: %+v", got)
	}
	if got.Settings["cron.database_name"] != "'fork'" {
		t.Fatalf("the retained metadata setting did not follow the rename: %+v", got.Settings)
	}
}

func cronRequirement(t *testing.T) catalog.Extension {
	t.Helper()
	ext, ok := catalog.Lookup("pg_cron")
	if !ok {
		t.Fatal("the built-in pg_cron definition is missing")
	}
	return ext
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

func TestVerifyCopyGuardUsesTypedSettingFacts(t *testing.T) {
	useTempGuardWriter(t)
	dir := t.TempDir()
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Settings: map[string]string{"work_mem": "'1MB'"},
	}); err != nil {
		t.Fatal(err)
	}
	fact := func(value string, pending bool) map[string]string {
		return map[string]string{"v": value, "vartype": "integer", "unit": "kB", "p": boolText(pending)}
	}

	t.Run("unit equivalent", func(t *testing.T) {
		fake := &guardFactFake{fact: fact("1024kB", false)}
		pending, err := verifyCopyGuard(context.Background(), fake, dir)
		if err != nil || len(pending) != 0 {
			t.Fatalf("equivalent unit = %v, %v", pending, err)
		}
		if len(fake.queries) != 1 || !strings.Contains(fake.queries[0], "AS unit") {
			t.Fatalf("the guard query did not request the setting unit: %v", fake.queries)
		}
		if strings.Contains(fake.queries[0], "current_setting('work_mem', true)") {
			t.Fatalf("the guard query used missing_ok: %s", fake.queries[0])
		}
	})
	t.Run("true mismatch", func(t *testing.T) {
		fake := &guardFactFake{fact: fact("2MB", false)}
		if _, err := verifyCopyGuard(context.Background(), fake, dir); err == nil || !strings.Contains(err.Error(), "work_mem") {
			t.Fatalf("typed mismatch = %v", err)
		}
	})
	t.Run("pending", func(t *testing.T) {
		fake := &guardFactFake{fact: fact("2MB", true)}
		pending, err := verifyCopyGuard(context.Background(), fake, dir)
		if err != nil || len(pending) != 1 || pending[0] != "work_mem" {
			t.Fatalf("pending typed mismatch = %v, %v", pending, err)
		}
	})
}

func TestVerifyCopyGuardKeepsCustomPlaceholderExact(t *testing.T) {
	useTempGuardWriter(t)
	dir := t.TempDir()
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Settings: map[string]string{"custom.placeholder": "'Mixed Case'"},
	}); err != nil {
		t.Fatal(err)
	}
	effective := &guardFactFake{fact: map[string]string{"v": "Mixed Case", "p": "false"}}
	if pending, err := verifyCopyGuard(context.Background(), effective, dir); err != nil || len(pending) != 0 {
		t.Fatalf("an unknown-typed placeholder did not compare exactly: %v, %v", pending, err)
	}
	changed := &guardFactFake{fact: map[string]string{"v": "mixed case", "p": "false"}}
	if _, err := verifyCopyGuard(context.Background(), changed, dir); err == nil {
		t.Fatal("an unknown-typed placeholder was compared case-insensitively")
	}
}

func TestVerifyCopyGuardRefusesMissingEmptyValuedSetting(t *testing.T) {
	useTempGuardWriter(t)
	dir := t.TempDir()
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Settings: map[string]string{"custom.empty": "''"},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &missingSettingFake{key: "custom.empty"}
	if _, err := verifyCopyGuard(context.Background(), fake, dir); err == nil {
		t.Fatal("a missing setting with an empty expected value was accepted")
	}
	if len(fake.queries) != 1 || !strings.Contains(fake.queries[0], "current_setting('custom.empty')") {
		t.Fatalf("the guard did not query the setting strictly: %v", fake.queries)
	}
}

// guardFactFake returns one setting fact regardless of the key: each test
// installs one setting, so the single query it receives is unambiguous.
type guardFactFake struct {
	fact    map[string]string
	queries []string
}

func (g *guardFactFake) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	g.queries = append(g.queries, sql)
	return []map[string]string{g.fact}, nil
}

func (g *guardFactFake) Exec(context.Context, string, string, map[string]string) error { return nil }

// missingSettingFake models PostgreSQL for a setting that does not exist: the
// missing_ok form would return an empty value, while the strict form errors.
type missingSettingFake struct {
	key     string
	queries []string
}

func (m *missingSettingFake) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	m.queries = append(m.queries, sql)
	if strings.Contains(sql, "current_setting("+extensionLiteral(m.key)+", true)") {
		return []map[string]string{{"v": "", "vartype": "", "unit": "", "p": "false"}}, nil
	}
	return nil, errors.New(`ERROR: unrecognized configuration parameter "` + m.key + `"`)
}

func (m *missingSettingFake) Exec(context.Context, string, string, map[string]string) error {
	return nil
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

func TestSettleRestoredCopySkipsAbsentCron(t *testing.T) {
	old := deactivateCopyCron
	deactivateCopyCron = deactivateCopiedCronJobs
	t.Cleanup(func() { deactivateCopyCron = old })
	fake := &cronFake{metadataDB: "app", metadataOID: "42", currentName: "app"}
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app", Stanza: "app-stanza"}
	plan := &backup.CopyPlan{Cron: true, Requirements: []catalog.Extension{cronRequirement(t)}}
	if _, err := settleRestoredCopy(context.Background(), cfg, spec, fake, t.TempDir(), plan, "app", nil); err != nil {
		t.Fatalf("a retained pg_cron that the restored data does not install blocked settlement: %v", err)
	}
	if fake.jobReads != 0 {
		t.Fatalf("proven absence still inspected cron.job: %d reads", fake.jobReads)
	}
}

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
	fake := &cronFake{metadataDB: "app", metadataOID: "42", currentName: "app_follower", installed: true,
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
