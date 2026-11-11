package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/maint"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/steps"
)

type fakeRepackPG struct {
	queries        []string
	dbs            []string
	answer         func(db, sql string) ([]map[string]string, error)
	execs          int
	storageVersion int
}

func (f *fakeRepackPG) Query(_ context.Context, db, sql string, _ map[string]string) ([]map[string]string, error) {
	f.queries = append(f.queries, sql)
	f.dbs = append(f.dbs, db)
	if f.answer != nil {
		return f.answer(db, sql)
	}
	return nil, nil
}

func (f *fakeRepackPG) Exec(context.Context, string, string, map[string]string) error {
	f.execs++
	return nil
}

func repackStorageAnswerFor(pg *fakeRepackPG, base func(db, sql string) ([]map[string]string, error)) func(db, sql string) ([]map[string]string, error) {
	return func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "relfilenode") && strings.Contains(sql, "pg_class") {
			return []map[string]string{{"oid": "1", "relfilenode": fmt.Sprint(100 + pg.storageVersion*100), "tablespace": "pg_default"}}, nil
		}
		return base(db, sql)
	}
}

func repackStorageAnswer(pg *fakeRepackPG) func(db, sql string) ([]map[string]string, error) {
	return repackStorageAnswerFor(pg, repackDefaultAnswer)
}

func repackTestConfig(t *testing.T) config.Cluster {
	t.Helper()
	return config.Cluster{
		ClusterName: "prod",
		Server:      config.Server{DataRoot: t.TempDir()},
		Maintenance: config.Maintenance{Window: "02:00-05:00", RepackBloatThreshold: 0.40},
		Clusters:    []config.ClusterSpec{{Name: "app", Port: 5433}},
	}
}

func repackTestNow() time.Time {
	return time.Date(2026, 10, 14, 3, 0, 0, 0, time.Local)
}

func repackDefaultAnswer(db, sql string) ([]map[string]string, error) {
	switch {
	case strings.Contains(sql, "extversion") && strings.Contains(sql, "pg_repack") && !strings.Contains(sql, "version_sql"):
		return []map[string]string{{"extversion": "1.5.3"}}, nil
	case strings.Contains(sql, "extversion") && strings.Contains(sql, "pgstattuple") && !strings.Contains(sql, "pg_proc"):
		return []map[string]string{{"extversion": "1.5"}}, nil
	case strings.Contains(sql, "pg_proc") && strings.Contains(sql, "version_sql"):
		return []map[string]string{{"schema": "repack"}}, nil
	case strings.Contains(sql, "pg_proc") && strings.Contains(sql, "pgstattuple"):
		return []map[string]string{{"schema": "public"}}, nil
	case strings.Contains(sql, "pg_extension") && strings.Contains(sql, "version_sql"):
		return []map[string]string{{"extension_version": "1.5.3", "library_version": "pg_repack 1.5.3", "sql_version": "pg_repack 1.5.3",
			"db_default_tablespace": "pg_default", "session_default_tablespace": ""}}, nil
	case strings.Contains(sql, "WITH RECURSIVE"):
		return nil, nil
	case strings.Contains(sql, "relfilenode") && strings.Contains(sql, "pg_class"):
		return []map[string]string{{"oid": "1", "relfilenode": "100", "tablespace": "pg_default"}}, nil
	case strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "dattablespace") && !strings.Contains(sql, "reltablespace"):
		return []map[string]string{{"tablespace": "pg_default"}}, nil
	case strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "reltablespace"):
		return []map[string]string{
			{"kind": "table", "name": "events", "tablespace": "pg_default"},
			{"kind": "index", "name": "events_pkey", "tablespace": "pg_default"},
		}, nil
	case strings.Contains(sql, "indisunique"):
		return []map[string]string{{"ok": "t"}}, nil
	case strings.Contains(sql, "pg_total_relation_size"):
		return []map[string]string{{"n": "1048576"}}, nil
	case strings.Contains(sql, "relkind"):
		return []map[string]string{{
			"oid": "1", "schema": "public", "table": "events", "kind": "r", "persistence": "p",
			"access_method": "heap", "is_partition": "f", "has_parent": "f", "system_schema": "f",
		}}, nil
	case strings.Contains(sql, "bloat_bytes"):
		return nil, nil
	case strings.Contains(sql, "pgstattuple") && strings.Contains(sql, "table_len"):
		return []map[string]string{{
			"table_len": "1000", "tuple_count": "10", "tuple_len": "700", "tuple_percent": "70",
			"dead_tuple_count": "2", "dead_tuple_len": "100", "dead_tuple_percent": "10",
			"free_space": "200", "free_percent": "20",
		}}, nil
	}
	return nil, nil
}

func installRepackSeams(t *testing.T, pg *fakeRepackPG) {
	t.Helper()
	oldPG, oldPriv, oldClient, oldSpace, oldExec := maintRepackPG, maintRepackPrivilegedPG, maintRepackClientVersion, maintRepackSpace, maintRepackExec
	oldReport, oldRunning, oldRecords, oldInstance := maintReportPG, clusterRunning, backup.RecordsPath, followHostInstance
	oldLocks, oldNow, oldReload := autoRepackLocks, autoRepackNow, reloadJobCluster
	maintRepackPG = func(config.ClusterSpec) sqlRunner { return pg }
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return pg }
	maintReportPG = func(config.ClusterSpec) sqlRunner { return pg }
	maintRepackClientVersion = func(context.Context) (string, error) { return "pg_repack 1.5.3", nil }
	maintRepackSpace = func(needs []backup.SpaceNeed) ([]backup.SpaceNeed, error) { return needs, nil }
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.execs++
		pg.storageVersion++
		return repackExecResult{}
	}
	clusterRunning = func(config.ClusterSpec) bool { return true }
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	followHostInstance = func() (string, error) { return "test-instance", nil }
	autoRepackLocks = func(context.Context, *jobEnv, config.ClusterSpec) (engine.Release, string, error) {
		return func() {}, "", nil
	}
	autoRepackNow = repackTestNow
	reloadJobCluster = func() (config.Cluster, error) { return repackTestConfig(t), nil }
	t.Cleanup(func() {
		maintRepackPG, maintRepackPrivilegedPG, maintRepackClientVersion, maintRepackSpace, maintRepackExec = oldPG, oldPriv, oldClient, oldSpace, oldExec
		maintReportPG, clusterRunning, backup.RecordsPath, followHostInstance = oldReport, oldRunning, oldRecords, oldInstance
		autoRepackLocks, autoRepackNow, reloadJobCluster = oldLocks, oldNow, oldReload
	})
}

func reloadRepackAs(t *testing.T, cfg config.Cluster) {
	t.Helper()
	reloadJobCluster = func() (config.Cluster, error) { return cfg, nil }
}

func TestMaintenancePGDefaults(t *testing.T) {
	report, ok := maintReportAdminPG(config.ClusterSpec{Port: 5433}).(pg.Local)
	if !ok || !report.AsRoot || report.User != "vaire_admin" || report.Host != steps.PGSocketDir || report.Port != 5433 {
		t.Fatalf("report default %+v", report)
	}
	admin, ok := maintRepackAdminPG(config.ClusterSpec{Port: 5433}).(pg.Local)
	if !ok || !admin.AsRoot || admin.User != "vaire_admin" {
		t.Fatalf("repack admin default %+v", admin)
	}
	postgres, ok := maintRepackPostgresPG(config.ClusterSpec{Port: 5433}).(pg.Local)
	if !ok || postgres.AsRoot || postgres.User != "" || postgres.Port != 5433 {
		t.Fatalf("repack postgres default %+v", postgres)
	}
}

func TestRepackVersionProbeUsesPrivilegedConnection(t *testing.T) {
	admin := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, ".version()") || strings.Contains(sql, ".version_sql()") {
			return nil, fmt.Errorf("permission denied for schema repack")
		}
		return repackDefaultAnswer(db, sql)
	}}
	priv := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	plan, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VersionCheck != "matched" {
		t.Fatalf("version check %q", plan.VersionCheck)
	}
	for _, q := range admin.queries {
		if strings.Contains(q, ".version()") || strings.Contains(q, ".version_sql()") {
			t.Fatalf("unprivileged connection called version functions: %s", q)
		}
	}
	found := false
	for _, q := range priv.queries {
		if strings.Contains(q, "version_sql") {
			found = true
		}
	}
	if !found {
		t.Fatalf("privileged probe did not call version_sql: %v", priv.queries)
	}
}

func TestRepackPreflightDefersVersionProbe(t *testing.T) {
	admin := &fakeRepackPG{answer: repackDefaultAnswer}
	priv := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	plan, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VersionCheck != "deferred_to_job" || len(priv.queries) != 0 {
		t.Fatalf("preflight probe: %q %v", plan.VersionCheck, priv.queries)
	}
}

func TestRepackProtectedNoPrivilegedProbe(t *testing.T) {
	admin := &fakeRepackPG{answer: repackDefaultAnswer}
	priv := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	cfg := repackTestConfig(t)
	cfg.Clusters[0].Protected = true
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonProtected {
		t.Fatalf("protected: %v", err)
	}
	if len(admin.queries) != 0 || len(priv.queries) != 0 {
		t.Fatalf("protected probe: admin %v privileged %v", admin.queries, priv.queries)
	}
}

func TestRepackPlanProtectedRefused(t *testing.T) {
	pg := &fakeRepackPG{}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cfg.Clusters[0].Protected = true
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonProtected {
		t.Fatalf("err %v", err)
	}
	if len(pg.queries) != 0 {
		t.Fatalf("protected preflight queried: %v", pg.queries)
	}
}

func TestRepackPlanLeavesAccepted(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	for _, facts := range []map[string]string{
		{"oid": "1", "schema": "public", "table": "events", "kind": "r", "persistence": "p", "access_method": "heap", "is_partition": "t", "has_parent": "t", "system_schema": "f"},
		{"oid": "1", "schema": "public", "table": "events", "kind": "r", "persistence": "p", "access_method": "heap", "is_partition": "f", "has_parent": "t", "system_schema": "f"},
	} {
		pg.answer = func(db, sql string) ([]map[string]string, error) {
			if strings.Contains(sql, "relkind") {
				return []map[string]string{facts}, nil
			}
			return repackDefaultAnswer(db, sql)
		}
		o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
		plan, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
		if err != nil {
			t.Fatalf("%v: %v", facts, err)
		}
		if len(plan.Tables) != 1 || plan.Tables[0] != "public.events" {
			t.Fatalf("%v: plan %+v", facts, plan)
		}
	}
}

func TestRepackPlanUnsupportedAndKey(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cases := []struct {
		name string
		sql  string
		row  map[string]string
		want string
	}{
		{"partition parent", "relkind", map[string]string{"schema": "public", "table": "events", "kind": "p", "persistence": "p", "access_method": "heap"}, maint.RepackReasonPartitionParent},
		{"foreign", "relkind", map[string]string{"schema": "public", "table": "events", "kind": "f", "persistence": "p"}, maint.RepackReasonForeignTable},
		{"non-heap", "relkind", map[string]string{"schema": "public", "table": "events", "kind": "r", "persistence": "p", "access_method": "columnar"}, maint.RepackReasonAccessMethod},
		{"extension storage", "WITH RECURSIVE", map[string]string{"extension": "timescaledb", "depth": "0", "scope": "schema"}, maint.RepackReasonExtensionStorage},
		{"no key", "indisunique", map[string]string{"ok": "f"}, maint.RepackReasonNoKey},
	}
	for _, tc := range cases {
		pg.answer = func(db, sql string) ([]map[string]string, error) {
			if strings.Contains(sql, tc.sql) {
				return []map[string]string{tc.row}, nil
			}
			return repackDefaultAnswer(db, sql)
		}
		o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
		_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
		if err == nil || reason(t, err) != tc.want {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestRepackPlanExternalTablespaceAndSpace(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "reltablespace") {
			return []map[string]string{{"kind": "index", "name": "events_pkey", "tablespace": "fastspace"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonTablespace {
		t.Fatalf("external index tablespace: %v", err)
	}
	pg.answer = func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "reltablespace") {
			return []map[string]string{{"kind": "table", "name": "events", "tablespace": "fastspace"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}
	_, err = planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonTablespace {
		t.Fatalf("external database-default tablespace: %v", err)
	}
	maintRepackSpace = func(needs []backup.SpaceNeed) ([]backup.SpaceNeed, error) {
		return []backup.SpaceNeed{{Path: "/data", Need: 100, Unknown: true}}, nil
	}
	pg.answer = repackDefaultAnswer
	_, err = planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonSpaceUnknown {
		t.Fatalf("unknown space: %v", err)
	}
	maintRepackSpace = func([]backup.SpaceNeed) ([]backup.SpaceNeed, error) {
		return nil, output.NewError(output.CodePrecondition, "not enough free space", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": maint.RepackReasonInsufficientSpace})
	}
	_, err = planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonInsufficientSpace {
		t.Fatalf("insufficient space: %v", err)
	}
}

func TestRepackPlanDefaultTablespaceExternalOrUnknown(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "dattablespace") && !strings.Contains(sql, "reltablespace") {
			return []map[string]string{{"tablespace": "fastspace"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), false)
	if err == nil || reason(t, err) != maint.RepackReasonDefaultTablespace {
		t.Fatalf("external database default: %v", err)
	}
	pg.answer = func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "dattablespace") && !strings.Contains(sql, "reltablespace") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}
	_, err = planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), false)
	if err == nil || reason(t, err) != maint.RepackReasonDefaultTablespace {
		t.Fatalf("unknown database default: %v", err)
	}
}

func TestRepackPlanSessionDefaultTablespaceExternal(t *testing.T) {
	admin := &fakeRepackPG{answer: repackDefaultAnswer}
	priv := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "session_default_tablespace") {
			return []map[string]string{{"extension_version": "1.5.3", "library_version": "pg_repack 1.5.3", "sql_version": "pg_repack 1.5.3",
				"db_default_tablespace": "pg_default", "session_default_tablespace": "fastspace"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonDefaultTablespace {
		t.Fatalf("external session default: %v", err)
	}
}

func TestRepackPlanTOASTExternalTablespace(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_tablespace") && strings.Contains(sql, "reltablespace") {
			return []map[string]string{{"kind": "toast", "name": "pg_toast_1", "tablespace": "fastspace"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonTablespace {
		t.Fatalf("external toast tablespace: %v", err)
	}
}

func TestRepackPlanWindowAndVersion(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events"}
	outside := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	if _, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, outside, true); err == nil || reason(t, err) != maint.RepackReasonOutsideWindow {
		t.Fatalf("outside window: %v", err)
	}
	o.Now = true
	if _, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, outside, true); err != nil {
		t.Fatalf("--now: %v", err)
	}
	maintRepackClientVersion = func(context.Context) (string, error) { return "pg_repack 1.5.1", nil }
	o.Now = true
	if _, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true); err == nil || reason(t, err) != maint.RepackReasonVersionMismatch {
		t.Fatalf("version mismatch: %v", err)
	}
}

func TestRepackPlanExtensionMissing(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "extversion") && strings.Contains(sql, "pg_repack") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonExtensionMissing {
		t.Fatalf("err %v", err)
	}
}

func TestRepackPlanNotRunningAndClientMissing(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	clusterRunning = func(config.ClusterSpec) bool { return false }
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonClusterNotRunning || len(pg.queries) != 0 || pg.execs != 0 {
		t.Fatalf("not running: %v queries %v", err, pg.queries)
	}
	clusterRunning = func(config.ClusterSpec) bool { return true }
	maintRepackClientVersion = func(context.Context) (string, error) { return "", fmt.Errorf("missing binary") }
	_, err = planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonClientMissing || pg.execs != 0 {
		t.Fatalf("client missing: %v", err)
	}
}

func TestRepackPlanVersionFactsUnknown(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "extversion") && strings.Contains(sql, "pg_repack") {
			return []map[string]string{{"extversion": "1.5.2"}}, nil
		}
		if strings.Contains(sql, "pg_proc") && strings.Contains(sql, "version_sql") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	_, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err == nil || reason(t, err) != maint.RepackReasonVersionMismatch || !strings.Contains(err.Error(), "version facts are unavailable") {
		t.Fatalf("err %v", err)
	}
}

func TestRepackPlanNoTableSkips(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "bloat_bytes") {
			return []map[string]string{
				{"kind": "table", "schemaname": "public", "table_name": "big", "index_name": "", "bloat_bytes": "104857600", "real_size": "209715200", "bloat_pct": "50", "fillfactor": "100", "is_na": "f"},
				{"kind": "table", "schemaname": "public", "table_name": "bad", "index_name": "", "bloat_bytes": "52428800", "real_size": "104857600", "bloat_pct": "50", "fillfactor": "100", "is_na": "f"},
			}, nil
		}
		if strings.Contains(sql, "relkind") && strings.Contains(sql, "bad") {
			return []map[string]string{{"schema": "public", "table": "bad", "kind": "p", "persistence": "p", "access_method": "heap"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Now: true}
	plan, err := planRepack(context.Background(), cfg, cfg.Clusters[0], &o, repackTestNow(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tables) != 1 || plan.Tables[0] != "public.big" || len(plan.Skipped) != 1 || plan.Skipped[0].Reason != maint.RepackReasonPartitionParent {
		t.Fatalf("%+v", plan)
	}
}

func TestJobRepackProtectedAndConfirm(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cfg.Clusters[0].Protected = true
	reloadRepackAs(t, cfg)
	env := &jobEnv{Cluster: cfg, Job: "maint", Params: json.RawMessage(`{"kind":"repack","db":"app","table":"public.events","now":true}`)}
	_, err := jobRepack(context.Background(), env, maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}, cfg.Clusters[0])
	if err == nil || reason(t, err) != maint.RepackReasonProtected {
		t.Fatalf("protected job: %v", err)
	}
	if len(pg.queries) != 0 {
		t.Fatalf("protected job queried: %v", pg.queries)
	}
}

func TestJobRepackReloadsProtectionAndIdentityUnderLocks(t *testing.T) {
	admin := &fakeRepackPG{answer: repackDefaultAnswer}
	priv := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	cfg := repackTestConfig(t)
	params := json.RawMessage(`{"kind":"repack","db":"app","table":"public.events","now":true}`)
	protected := repackTestConfig(t)
	protected.Clusters[0].Protected = true
	reloadRepackAs(t, protected)
	_, err := jobMaint(context.Background(), &jobEnv{Cluster: cfg, Job: "maint", Params: params})
	if err == nil || reason(t, err) != maint.RepackReasonProtected {
		t.Fatalf("protection flip: %v", err)
	}
	if len(admin.queries) != 0 || len(priv.queries) != 0 || admin.execs != 0 {
		t.Fatalf("protection flip effects: admin %v privileged %v execs %d", admin.queries, priv.queries, admin.execs)
	}
	changed := repackTestConfig(t)
	changed.Clusters[0].Port = 5434
	reloadRepackAs(t, changed)
	_, err = jobMaint(context.Background(), &jobEnv{Cluster: cfg, Job: "maint", Params: params})
	if err == nil || reason(t, err) != "config_changed" {
		t.Fatalf("identity change: %v", err)
	}
	if len(admin.queries) != 0 || len(priv.queries) != 0 || admin.execs != 0 {
		t.Fatalf("identity change effects: admin %v privileged %v execs %d", admin.queries, priv.queries, admin.execs)
	}
	changedStanza := repackTestConfig(t)
	changedStanza.Clusters[0].Stanza = "app-abcdef"
	reloadRepackAs(t, changedStanza)
	_, err = jobMaint(context.Background(), &jobEnv{Cluster: cfg, Job: "maint", Params: params})
	if err == nil || reason(t, err) != "config_changed" {
		t.Fatalf("stanza change: %v", err)
	}
	if len(admin.queries) != 0 || len(priv.queries) != 0 || admin.execs != 0 {
		t.Fatalf("stanza change effects: admin %v privileged %v execs %d", admin.queries, priv.queries, admin.execs)
	}
}

func TestRepackSkipRequiresIdenticalValidStorage(t *testing.T) {
	before := maint.RepackStorage{OID: 42, RelFileNode: 100, TableSpace: "pg_default"}
	res := repackExecResult{Output: "INFO: Skipping repack public.events due to timeout"}
	for _, tc := range []struct {
		name  string
		after maint.RepackStorage
		err   error
		want  string
	}{
		{"unchanged", before, nil, maint.RepackOutcomeSkipped},
		{"oid changed", maint.RepackStorage{OID: 43, RelFileNode: 100, TableSpace: "pg_default"}, nil, maint.RepackOutcomeUnknown},
		{"tablespace changed", maint.RepackStorage{OID: 42, RelFileNode: 100, TableSpace: "external"}, nil, maint.RepackOutcomeUnknown},
		{"storage changed", maint.RepackStorage{OID: 42, RelFileNode: 101, TableSpace: "pg_default"}, nil, maint.RepackOutcomeUnknown},
		{"invalid identity", maint.RepackStorage{}, nil, maint.RepackOutcomeUnknown},
		{"unreadable", before, fmt.Errorf("catalog unavailable"), maint.RepackOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := classifyRepackCompletion(before, res, tc.after, tc.err)
			if got != tc.want {
				t.Fatalf("outcome = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJobRepackCompletionEvidence(t *testing.T) {
	pg := &fakeRepackPG{}
	pg.answer = repackStorageAnswer(pg)
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	o := maint.Options{Kind: maint.Repack, DB: "app", Table: "public.events", Now: true}
	env := &jobEnv{Cluster: cfg, Job: "maint", Params: json.RawMessage(`{"kind":"repack","db":"app","table":"public.events","now":true}`)}
	run := func(t *testing.T) (map[string]any, error) {
		t.Helper()
		res, err := jobRepack(context.Background(), env, o, cfg.Clusters[0])
		if res == nil {
			return nil, err
		}
		return res.(map[string]any), err
	}
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.storageVersion++
		return repackExecResult{}
	}
	data, err := run(t)
	if err != nil {
		t.Fatal(err)
	}
	if counts := data["counts"].(maint.RepackCounts); counts.Unknown != 1 || counts.Repacked != 0 {
		t.Fatalf("clean exit must be unknown: %+v", data)
	}
	if data["unknown"].(map[string]string)["public.events"] == "" {
		t.Fatalf("unknown detail missing: %+v", data)
	}
	pg.storageVersion = 0
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		return repackExecResult{Output: "INFO: repacking table \"public.events\"\nINFO: Skipping repack public.events due to timeout"}
	}
	data, err = run(t)
	if err != nil {
		t.Fatal(err)
	}
	skips := data["skipped"].([]maint.RepackSkip)
	if len(skips) != 1 || skips[0].Reason != maint.RepackReasonLockTimeout {
		t.Fatalf("zero-exit timeout: %+v", data)
	}
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		return repackExecResult{Output: "WARNING: Invalid index: CREATE UNIQUE INDEX events_bad ON public.events"}
	}
	data, err = run(t)
	if err != nil {
		t.Fatal(err)
	}
	skips = data["skipped"].([]maint.RepackSkip)
	if len(skips) != 1 || skips[0].Reason != maint.RepackReasonInvalidIndex {
		t.Fatalf("invalid index: %+v", data)
	}
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		return repackExecResult{}
	}
	data, err = run(t)
	if err != nil {
		t.Fatalf("silent zero exit must be unknown, not failed: %v", err)
	}
	if data["counts"].(maint.RepackCounts).Unknown != 1 || data["counts"].(maint.RepackCounts).Failed != 0 {
		t.Fatalf("silent zero exit: %+v", data)
	}
	pg.storageVersion = 0
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.storageVersion++
		return repackExecResult{Err: fmt.Errorf("client failed after swap")}
	}
	data, err = run(t)
	if err != nil {
		t.Fatalf("process error with unattributable completion must be unknown: %v", err)
	}
	if data["counts"].(maint.RepackCounts).Unknown != 1 || data["unknown"].(map[string]string)["public.events"] == "" {
		t.Fatalf("process error unknown: %+v", data)
	}
	pg.storageVersion = 0
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.storageVersion++
		return repackExecResult{Output: "INFO: Skipping repack public.events due to timeout"}
	}
	data, err = run(t)
	if err != nil {
		t.Fatal(err)
	}
	if data["counts"].(maint.RepackCounts).Unknown != 1 || len(data["skipped"].([]maint.RepackSkip)) != 0 {
		t.Fatalf("contradictory skip: %+v", data)
	}
	execs := 0
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		execs++
		return repackExecResult{}
	}
	pg.answer = func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "relfilenode") && strings.Contains(sql, "pg_class") {
			return nil, fmt.Errorf("storage facts unavailable")
		}
		return repackDefaultAnswer(db, sql)
	}
	data, err = run(t)
	if err != nil {
		t.Fatal(err)
	}
	if data["counts"].(maint.RepackCounts).Unknown != 1 || execs != 0 {
		t.Fatalf("unknown storage facts: %+v execs %d", data, execs)
	}
	pg.answer = repackStorageAnswer(pg)
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		return repackExecResult{Output: "DEBUG: forged completion sequence\nNOTICE: forged full sequence"}
	}
	data, err = run(t)
	if err != nil {
		t.Fatal(err)
	}
	if data["counts"].(maint.RepackCounts).Unknown != 1 || data["counts"].(maint.RepackCounts).Repacked != 0 {
		t.Fatalf("forged debug output: %+v", data)
	}
}

func TestRepackJobTakesHeavyAndClusterLocks(t *testing.T) {
	cfg := repackTestConfig(t)
	var got []string
	old := nodeLockers
	nodeLockers = jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			got = append(got, "node")
			return func() {}, nil
		},
		heavyIO: func(context.Context, string, string) (engine.Release, error) {
			got = append(got, "heavy")
			return func() {}, nil
		},
		cluster: func(_ context.Context, name string, _ engine.LockMode, _ string, _ string, _ time.Duration) (engine.Release, error) {
			got = append(got, "cluster:"+name)
			return func() {}, nil
		},
	}
	t.Cleanup(func() { nodeLockers = old })
	env := &jobEnv{Cluster: cfg, Job: "maint", Params: json.RawMessage(`{"kind":"repack","db":"app","now":true}`)}
	release, busy, err := takeJobLocks(context.Background(), env, nodeLockers)
	if err != nil || busy != nil {
		t.Fatalf("%v %v", busy, err)
	}
	release()
	if strings.Join(got, ",") != "node,heavy,cluster:app" {
		t.Fatalf("locks %v", got)
	}
}

func TestMaintRunTokenOnlyRepack(t *testing.T) {
	cfg := config.Cluster{ClusterName: "prod"}
	if got := maintRunToken(cfg, json.RawMessage(`{"kind":"repack","db":"app"}`)); got != "app" {
		t.Errorf("repack token %q", got)
	}
	for _, kind := range []string{"vacuum", "analyze", "reindex"} {
		if got := maintRunToken(cfg, json.RawMessage(`{"kind":"`+kind+`","db":"app"}`)); got != "" {
			t.Errorf("%s token %q", kind, got)
		}
	}
}

func TestMaintRunProtectedAndConfirm(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cfg.Clusters[0].Protected = true
	reloadRepackAs(t, cfg)
	req := request("maint.run", map[string]any{"db": "app", "kind": "repack", "now": true})
	_, err := handleMaintRun(context.Background(), &Env{Cluster: cfg, Req: req})
	if err == nil || reason(t, err) != maint.RepackReasonProtected {
		t.Fatalf("protected: %v", err)
	}
	cfg.Clusters[0].Protected = false
	_, err = handleMaintRun(context.Background(), &Env{Cluster: cfg, Req: request("maint.run", map[string]any{"db": "app", "kind": "repack", "now": true})})
	var ce *output.CLIError
	if !errors.As(err, &ce) || ce.Code != output.CodeUnsafeAction {
		t.Fatalf("confirm: %v", err)
	}
	if pg.execs != 0 {
		t.Fatal("missing confirmation executed pg_repack")
	}
}

func TestMaintenanceRefusesActiveFence(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	reloadRepackAs(t, cfg)
	fence := backup.ActivationFence{
		OperationID: "op", Kind: "fork", Database: "app", Instance: "test-instance",
		TargetStanza: "stanza", OldStanza: "old", NewStanza: "new",
		Registration: "app:5433:", Port: 5433,
		DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"), Phase: "prepared",
	}
	if err := backup.SetActivationFence(fence); err != nil {
		t.Fatal(err)
	}
	_, err := handleMaintRun(context.Background(), &Env{Cluster: cfg, Req: request("maint.run", map[string]any{"db": "app", "kind": "vacuum"})})
	if err == nil || reason(t, err) != "activation_fence_active" {
		t.Fatalf("maint run: %v", err)
	}
	_, err = handleMaintKill(context.Background(), &Env{Cluster: cfg, Req: request("maint.kill", map[string]any{"db": "app", "pid": 42})})
	if err == nil || reason(t, err) != "activation_fence_active" {
		t.Fatalf("maint kill: %v", err)
	}
	_, err = jobMaint(context.Background(), &jobEnv{Cluster: cfg, Job: "maint", Params: json.RawMessage(`{"kind":"vacuum","db":"app"}`)})
	if err == nil || reason(t, err) != "activation_fence_active" {
		t.Fatalf("job maint: %v", err)
	}
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{{Kind: "table", Schema: "public", Table: "events", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5}}}}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Skipped != 1 || runs["app"].Skipped[0].Reason != maint.RepackReasonActivationFence {
		t.Fatalf("auto fence: %+v", runs["app"])
	}
}

func TestExactMaintReport(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	res, err := handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app", "table": "public.events", "exact": true,
	})})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := res.Data.(map[string]any)
	dbs, _ := data["databases"].([]maintReportDB)
	if len(dbs) != 1 || dbs[0].Exact == nil || dbs[0].Exact.TableBytes != 1000 || dbs[0].Exact.FreePercent != 20 {
		t.Fatalf("%+v", data)
	}
	if len(pg.dbs) == 0 {
		t.Fatal("no query")
	}
	for _, db := range pg.dbs {
		if db != "app" {
			t.Errorf("query db %q", db)
		}
	}
	foundScan, foundSchema, readOnly := false, false, false
	for _, q := range pg.queries {
		if strings.Contains(q, "pgstattuple") && !strings.Contains(q, "pgstattuple_approx") && strings.Contains(q, "table_len") {
			foundScan = true
			readOnly = strings.Contains(q, "default_transaction_read_only = on") &&
				strings.Contains(q, "lock_timeout = '5s'") &&
				strings.Contains(q, "statement_timeout = '60s'")
		}
		if strings.Contains(q, "proname = 'pgstattuple'") {
			foundSchema = true
		}
	}
	if !foundScan || !foundSchema || !readOnly {
		t.Fatalf("exact queries scan=%v schema=%v readonly=%v: %v", foundScan, foundSchema, readOnly, pg.queries)
	}
	if pg.execs != 0 {
		t.Fatal("exact report executed SQL")
	}
}

func TestExactMaintReportFollowerUsesSourceDB(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cfg.Clusters = []config.ClusterSpec{{Name: "app_follower", Follows: "app", Port: 5434}}
	res, err := handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app_follower", "table": "public.events", "exact": true,
	})})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := res.Data.(map[string]any)
	dbs, _ := data["databases"].([]maintReportDB)
	if len(dbs) != 1 || dbs[0].DB != "app_follower" || dbs[0].Exact == nil {
		t.Fatalf("%+v", data)
	}
	for _, db := range pg.dbs {
		if db != "app" {
			t.Fatalf("follower query used %q, want app", db)
		}
	}
}

func TestExactMaintReportMissingAndUnsupported(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "extversion") && strings.Contains(sql, "pgstattuple") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	_, err := handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app", "table": "public.events", "exact": true,
	})})
	if err == nil || reason(t, err) != maint.RepackReasonExtensionMissing {
		t.Fatalf("missing: %v", err)
	}
	pg.answer = func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "relkind") {
			return []map[string]string{{"schema": "public", "table": "events", "kind": "p", "persistence": "p", "access_method": "heap"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}
	_, err = handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app", "table": "public.events", "exact": true,
	})})
	if err == nil || reason(t, err) != maint.RepackReasonPartitionParent {
		t.Fatalf("unsupported: %v", err)
	}
	pg.answer = func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pg_proc") && strings.Contains(sql, "pgstattuple") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}
	_, err = handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app", "table": "public.events", "exact": true,
	})})
	if err == nil || reason(t, err) != "exact_bloat_failed" || !strings.Contains(err.Error(), "function schema is unavailable") {
		t.Fatalf("unknown function schema: %v", err)
	}
}

func TestExactMaintReportFlagsAndErrors(t *testing.T) {
	cfg := repackTestConfig(t)
	_, err := handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{"kind": "bloat", "db": "app", "exact": true})})
	if err == nil {
		t.Fatal("missing table passed")
	}
	_, err = handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{"kind": "bloat", "db": "app", "table": "events"})})
	if err == nil {
		t.Fatal("table without exact passed")
	}
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "pgstattuple") && strings.Contains(sql, "table_len") {
			return nil, fmt.Errorf("permission denied for function pgstattuple")
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	_, err = handleMaintReport(context.Background(), &Env{Cluster: cfg, Req: request("maint.report", map[string]any{
		"kind": "bloat", "db": "app", "table": "events", "exact": true,
	})})
	var ce *output.CLIError
	if err == nil || reason(t, err) != "exact_bloat_failed" || !errors.As(err, &ce) || !strings.Contains(ce.Hint, "does not escalate") {
		t.Fatalf("query error: %v", err)
	}
}

func TestAutoRepackOrderAndCounts(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	pg.answer = repackStorageAnswer(pg)
	var order []string
	maintRepackExec = func(_ context.Context, _ *jobEnv, _ config.ClusterSpec, table string) repackExecResult {
		order = append(order, table)
		pg.storageVersion++
		return repackExecResult{}
	}
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{
		{Kind: "table", Schema: "public", Table: "small", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5},
		{Kind: "table", Schema: "public", Table: "big", BloatBytes: 2 * maint.AutoRepackMinBytes, Ratio: 0.6},
	}}}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if strings.Join(order, ",") != "public.big,public.small" {
		t.Fatalf("order %v", order)
	}
	if runs["app"].Counts.Unknown != 2 || runs["app"].Counts.Repacked != 0 || runs["app"].Counts.Selected != 2 {
		t.Fatalf("%+v", runs["app"])
	}
}

func TestAutoRepackProtectedAndBusy(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	cfg.Clusters[0].Protected = true
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{{Kind: "table", Schema: "public", Table: "events", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5}}}}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Skipped != 1 || runs["app"].Skipped[0].Reason != maint.RepackReasonProtected || len(pg.queries) != 0 {
		t.Fatalf("protected: %+v queries %v", runs["app"], pg.queries)
	}
	cfg.Clusters[0].Protected = false
	autoRepackLocks = func(context.Context, *jobEnv, config.ClusterSpec) (engine.Release, string, error) {
		return nil, maint.RepackReasonHeavyIOBusy, nil
	}
	runs = autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Busy != 1 || runs["app"].Busy[0].Reason != maint.RepackReasonHeavyIOBusy {
		t.Fatalf("heavy busy: %+v", runs["app"])
	}
	autoRepackLocks = func(context.Context, *jobEnv, config.ClusterSpec) (engine.Release, string, error) {
		return nil, maint.RepackReasonClusterBusy, nil
	}
	runs = autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Busy != 1 || runs["app"].Busy[0].Reason != maint.RepackReasonClusterBusy {
		t.Fatalf("cluster busy: %+v", runs["app"])
	}
}

func TestAutoRepackWindowStopsNewTables(t *testing.T) {
	pg := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	var calls int
	autoRepackNow = func() time.Time {
		calls++
		if calls <= 2 {
			return repackTestNow()
		}
		return repackTestNow().Add(3 * time.Hour)
	}
	pg.answer = repackStorageAnswer(pg)
	var ran []string
	maintRepackExec = func(_ context.Context, _ *jobEnv, _ config.ClusterSpec, table string) repackExecResult {
		ran = append(ran, table)
		pg.storageVersion++
		return repackExecResult{}
	}
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{
		{Kind: "table", Schema: "public", Table: "first", BloatBytes: 2 * maint.AutoRepackMinBytes, Ratio: 0.5},
		{Kind: "table", Schema: "public", Table: "second", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5},
	}}}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if strings.Join(ran, ",") != "public.first" || runs["app"].Counts.Skipped != 1 || runs["app"].Skipped[0].Reason != maint.RepackReasonWindowEnded {
		t.Fatalf("ran %v run %+v", ran, runs["app"])
	}
}

func TestAutoRepackCompletionSkipsAndUnknownCounts(t *testing.T) {
	pg := &fakeRepackPG{}
	pg.answer = repackStorageAnswer(pg)
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{
		{Kind: "table", Schema: "public", Table: "events", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5},
	}}}
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		return repackExecResult{Output: "INFO: Skipping repack public.events due to timeout"}
	}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Skipped != 1 || runs["app"].Skipped[0].Reason != maint.RepackReasonLockTimeout || runs["app"].Counts.Repacked != 0 {
		t.Fatalf("auto zero-exit skip: %+v", runs["app"])
	}
	pg.storageVersion = 0
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.storageVersion++
		return repackExecResult{Err: fmt.Errorf("client failed after swap")}
	}
	runs = autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if runs["app"].Counts.Unknown != 1 || runs["app"].Counts.Repacked != 0 || runs["app"].Unknown["public.events"] == "" {
		t.Fatalf("auto process error with unattributable completion: %+v", runs["app"])
	}
}

func TestAutoRepackWindowExpiresDuringStorageProbe(t *testing.T) {
	storageQueried := false
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "relfilenode") && strings.Contains(sql, "pg_class") {
			storageQueried = true
			return []map[string]string{{"oid": "1", "relfilenode": "100", "tablespace": "pg_default"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	cfg := repackTestConfig(t)
	autoRepackNow = func() time.Time {
		if storageQueried {
			return repackTestNow().Add(3 * time.Hour)
		}
		return repackTestNow()
	}
	execs, releases := 0, 0
	autoRepackLocks = func(context.Context, *jobEnv, config.ClusterSpec) (engine.Release, string, error) {
		return func() { releases++ }, "", nil
	}
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		execs++
		return repackExecResult{}
	}
	reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{
		{Kind: "table", Schema: "public", Table: "first", BloatBytes: 2 * maint.AutoRepackMinBytes, Ratio: 0.5},
		{Kind: "table", Schema: "public", Table: "second", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5},
	}}}
	runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
	if !storageQueried {
		t.Fatal("storage probe never ran, so the clock did not advance during it")
	}
	if execs != 0 || releases != 1 || runs["app"].Counts.Repacked != 0 || runs["app"].Counts.Unknown != 0 ||
		runs["app"].Counts.Skipped != 2 {
		t.Fatalf("execs %d releases %d run %+v", execs, releases, runs["app"])
	}
	for _, skip := range runs["app"].Skipped {
		if skip.Reason != maint.RepackReasonWindowEnded {
			t.Fatalf("window expiry skip reason: %+v", runs["app"].Skipped)
		}
	}
}

func TestAutoRepackReloadIdentityStopsRemaining(t *testing.T) {
	variants := []struct {
		name   string
		mutate func(*config.ClusterSpec)
	}{
		{"port", func(s *config.ClusterSpec) { s.Port = 5434 }},
		{"stanza", func(s *config.ClusterSpec) { s.Stanza = "app-abcdef" }},
		{"follow generation", func(s *config.ClusterSpec) { s.FollowGeneration = "0123456789abcdef0123456789abcdef" }},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			pg := &fakeRepackPG{answer: repackDefaultAnswer}
			installRepackSeams(t, pg)
			cfg := repackTestConfig(t)
			changed := repackTestConfig(t)
			variant.mutate(&changed.Clusters[0])
			reloadRepackAs(t, changed)
			reports := map[string]maint.BloatReport{"app": {Rows: []maint.BloatRow{
				{Kind: "table", Schema: "public", Table: "first", BloatBytes: 2 * maint.AutoRepackMinBytes, Ratio: 0.5},
				{Kind: "table", Schema: "public", Table: "second", BloatBytes: maint.AutoRepackMinBytes, Ratio: 0.5},
			}}}
			runs := autoRepack(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}, reports, repackTestNow())
			if runs["app"].Counts.Skipped != 2 || runs["app"].Skipped[0].Reason != "config_changed" || pg.execs != 0 {
				t.Fatalf("identity reload: %+v execs %d", runs["app"], pg.execs)
			}
		})
	}
}

func TestMaintWeeklyReloadIdentity(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "bloat_bytes") {
			return nil, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	var ports []int
	maintReportPG = func(spec config.ClusterSpec) sqlRunner {
		ports = append(ports, spec.Port)
		return pg
	}
	oldPath := maintStatePath
	maintStatePath = filepath.Join(t.TempDir(), "maint.json")
	t.Cleanup(func() { maintStatePath = oldPath })
	cfg := repackTestConfig(t)
	changed := repackTestConfig(t)
	changed.Clusters[0].Port = 5434
	reloadRepackAs(t, changed)
	if _, err := jobMaintWeekly(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"}); err != nil {
		t.Fatal(err)
	}
	if len(ports) != 1 || ports[0] != 5434 {
		t.Fatalf("weekly report ports %v", ports)
	}
}

func TestMaintWeeklyReloadsProtectionUnderLock(t *testing.T) {
	admin := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "bloat_bytes") {
			return []map[string]string{{"kind": "table", "schemaname": "public", "table_name": "events", "index_name": "",
				"bloat_bytes": "104857600", "real_size": "209715200", "bloat_pct": "50", "fillfactor": "100", "is_na": "f"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	priv := &fakeRepackPG{answer: repackDefaultAnswer}
	installRepackSeams(t, admin)
	maintRepackPrivilegedPG = func(config.ClusterSpec) sqlRunner { return priv }
	oldPath := maintStatePath
	maintStatePath = filepath.Join(t.TempDir(), "maint.json")
	t.Cleanup(func() { maintStatePath = oldPath })
	cfg := repackTestConfig(t)
	cfg.Maintenance.AutoRepack = true
	protected := repackTestConfig(t)
	protected.Maintenance.AutoRepack = true
	protected.Clusters[0].Protected = true
	reloadRepackAs(t, protected)
	res, err := jobMaintWeekly(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"})
	if err != nil {
		t.Fatal(err)
	}
	runs, ok := res.(map[string]any)["repack"].(map[string]maint.RepackRun)
	if !ok || runs["app"].Counts.Skipped != 1 || runs["app"].Skipped[0].Reason != maint.RepackReasonProtected {
		t.Fatalf("protected reload: %+v", res)
	}
	if len(priv.queries) != 0 || admin.execs != 0 {
		t.Fatalf("protected reload privileged queries %v execs %d", priv.queries, admin.execs)
	}
}

func TestMaintWeeklyAutoRepackDisabledClearsState(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "bloat_bytes") {
			return []map[string]string{{"kind": "table", "schemaname": "public", "table_name": "events", "index_name": "",
				"bloat_bytes": "104857600", "real_size": "209715200", "bloat_pct": "50", "fillfactor": "100", "is_na": "f"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	oldPath := maintStatePath
	maintStatePath = filepath.Join(t.TempDir(), "maint.json")
	t.Cleanup(func() { maintStatePath = oldPath })
	cfg := repackTestConfig(t)
	cfg.Maintenance.AutoRepack = false
	reloadRepackAs(t, cfg)
	res, err := jobMaintWeekly(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(map[string]any)["repack"]; ok {
		t.Fatal("disabled auto-repack returned a repack result")
	}
	raw, _ := os.ReadFile(maintStatePath) //nolint:gosec // G304: test-owned temporary state path.
	if strings.Contains(string(raw), `"repack"`) {
		t.Fatalf("disabled auto-repack wrote repack state: %s", raw)
	}
}

func TestMaintWeeklyAutoRepackEnabledRecordsCounts(t *testing.T) {
	pg := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "bloat_bytes") {
			return []map[string]string{{"kind": "table", "schemaname": "public", "table_name": "events", "index_name": "",
				"bloat_bytes": "104857600", "real_size": "209715200", "bloat_pct": "50", "fillfactor": "100", "is_na": "f"}}, nil
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, pg)
	oldPath := maintStatePath
	maintStatePath = filepath.Join(t.TempDir(), "maint.json")
	t.Cleanup(func() { maintStatePath = oldPath })
	cfg := repackTestConfig(t)
	cfg.Maintenance.AutoRepack = true
	reloadRepackAs(t, cfg)
	baseAnswer := pg.answer
	pg.answer = repackStorageAnswerFor(pg, baseAnswer)
	maintRepackExec = func(context.Context, *jobEnv, config.ClusterSpec, string) repackExecResult {
		pg.storageVersion++
		return repackExecResult{}
	}
	res, err := jobMaintWeekly(context.Background(), &jobEnv{Cluster: cfg, Job: "maint-weekly"})
	if err != nil {
		t.Fatal(err)
	}
	runs, ok := res.(map[string]any)["repack"].(map[string]maint.RepackRun)
	if !ok || runs["app"].Counts.Unknown != 1 || runs["app"].Counts.Repacked != 0 || runs["app"].Counts.Selected != 1 {
		t.Fatalf("repack result %+v", res)
	}
	if runs["app"].Unknown["public.events"] == "" {
		t.Fatalf("repack result missing unknown detail %+v", runs["app"])
	}
	raw, _ := os.ReadFile(maintStatePath) //nolint:gosec // G304: test-owned temporary state path.
	if !strings.Contains(string(raw), `"unknown": 1`) || strings.Contains(string(raw), `"repacked": 1`) {
		t.Fatalf("state missing unknown repack counts: %s", raw)
	}
	var st maintState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	out := string(promfile.Render(maintFamilies(st)))
	if !strings.Contains(out, `vaire_maint_repack_tables{db="app",outcome="unknown"} 1`) {
		t.Fatalf("state did not round-trip to the unknown metric: %s", out)
	}
}

func TestMaintRepackFamilies(t *testing.T) {
	st := maintState{Repack: map[string]maint.RepackCounts{"app": {Repacked: 1, Failed: 2, Unknown: 5, Skipped: 3, Busy: 4}}}
	out := string(promfile.Render(maintFamilies(st)))
	for _, want := range []string{
		`vaire_maint_repack_tables{db="app",outcome="repacked"} 1`,
		`vaire_maint_repack_tables{db="app",outcome="failed"} 2`,
		`vaire_maint_repack_tables{db="app",outcome="unknown"} 5`,
		`vaire_maint_repack_tables{db="app",outcome="skipped"} 3`,
		`vaire_maint_repack_tables{db="app",outcome="busy"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestRepackActivityReasonsAreSafeTokens(t *testing.T) {
	for _, reason := range []string{
		maint.RepackReasonProtected, maint.RepackReasonNoKey, maint.RepackReasonVersionMismatch,
		maint.RepackReasonUnsupportedRelation, maint.RepackReasonPartitionParent, maint.RepackReasonForeignTable,
		maint.RepackReasonPersistence, maint.RepackReasonAccessMethod, maint.RepackReasonExtensionStorage,
		maint.RepackReasonSystemRelation, maint.RepackReasonRelationNotFound, maint.RepackReasonInsufficientSpace,
		maint.RepackReasonTablespace, maint.RepackReasonDefaultTablespace, maint.RepackReasonSpaceUnknown,
		maint.RepackReasonWindowEnded, maint.RepackReasonHeavyIOBusy, maint.RepackReasonClusterBusy,
		maint.RepackReasonLockTimeout, maint.RepackReasonInvalidIndex, maint.RepackReasonUnsafeDDL,
		maint.RepackReasonCompletionUnknown,
		"exact_bloat_failed", "activation_fence_active",
	} {
		if got := safeActivityToken(reason); got != reason {
			t.Errorf("%s became %q", reason, got)
		}
	}
}

func TestIncompleteActivityMappingRemainsUnknown(t *testing.T) {
	outcome, code, reason := activityStepOutcome("incomplete", "")
	if outcome != "unknown" || code != "" || reason != "incomplete" {
		t.Fatalf("incomplete mapping: %q %q %q", outcome, code, reason)
	}
}

func TestMaintStateRepackBackwardShape(t *testing.T) {
	var st maintState
	if err := json.Unmarshal([]byte(`{"last_weekly":"2026-10-14T02:00:00Z"}`), &st); err != nil {
		t.Fatal(err)
	}
	if st.Repack != nil {
		t.Fatalf("legacy JSON gained a repack map: %+v", st.Repack)
	}
}
