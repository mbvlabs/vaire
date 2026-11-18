package steps

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestAwaitingRecoverOnlyRoutesInspectionAlias(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Port: 5433}, {Name: "other", Port: 5434}}}

	recs := backup.Records{
		AwaitingRecovery: map[string]backup.Awaiting{"app": {}},
		Restores: map[string]backup.Restore{
			"app": {ID: "id", Database: "app", Phase: backup.PhaseReady, Port: 6433, Recover: true},
		},
	}
	for _, session := range []bool{false, true} {
		got := poolerDatabases(cfg, recs, session)

		var app, inspection, other bool

		for _, db := range got {
			switch db.Name {
			case "app":
				app = true
			case backup.RestoringName("app"):
				inspection = true

				if db.DBName != "app" || db.Port != 6433 {
					t.Fatalf("wrong inspection target: %+v", db)
				}
			case "other":
				other = true
			}
		}

		if app || !other || inspection != session {
			t.Fatalf("session=%v app=%v inspection=%v other=%v", session, app, inspection, other)
		}
	}
}

// A rebuilt master's first recovery runs pgbouncer-v1 while every database
// awaits recovery. The kept recovery target is still awaiting and its
// prepared activation fence withholds both the target and the ready copy's
// inspection alias; no other database is routable either.
func TestFirstRecoveryPoolersRenderEveryAliasWithheld(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Port: 5433}, {Name: "other", Port: 5434}}}
	recs := backup.Records{
		AwaitingRecovery: map[string]backup.Awaiting{"app": {}, "other": {}},
		Restores: map[string]backup.Restore{
			"app": {ID: "recover-1", Database: "app", Phase: backup.PhaseReady, Port: 6001, Recover: true},
		},
		Activation: map[string]backup.ActivationFence{
			"app": {OperationID: "recover-1", Kind: "restore-promote", Database: "app", Phase: "prepared"},
		},
	}
	view := cfg.RoutingView(recs.IsAwaiting, "app")

	files := pgbouncerFilesFrom(view, engine.Runtime{Host: hostinfo.Info{VCPU: 2}}, recs)
	if len(files) != 5 {
		t.Fatalf("pooler files = %d, want both poolers' complete set", len(files))
	}

	for _, f := range files {
		for _, alias := range []string{"app =", "app__auth", "app__restoring", "other ="} {
			if strings.Contains(f.content, alias) {
				t.Fatalf("%s rendered withheld %q:\n%s", f.path, alias, f.content)
			}
		}
	}

	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldRecords })

	if err := backup.UpdateRecords(func(s *backup.Records) error { *s = recs; return nil }); err != nil {
		t.Fatal(err)
	}

	if got := (pgbouncer{}).SkipReason(cfg.RoutingView(recs.IsAwaiting, "")); got != "" {
		t.Fatalf("a ready first recovery was skipped: %q", got)
	}

	init := backup.Records{AwaitingRecovery: recs.AwaitingRecovery}

	if err := backup.UpdateRecords(func(s *backup.Records) error { *s = init; return nil }); err != nil {
		t.Fatal(err)
	}

	if got := (pgbouncer{}).SkipReason(cfg.RoutingView(init.IsAwaiting, "")); got != "no_databases" {
		t.Fatalf("init skip reason = %q, want no_databases", got)
	}
}

// The service role verifiers come from the ready copy, never from the
// not-yet-created live cluster or its withheld route.
func TestRecoveryRoleVerifierSourceUsesReadyCopy(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Port: 5433}}}

	recs := backup.Records{
		AwaitingRecovery: map[string]backup.Awaiting{"app": {}},
		Restores: map[string]backup.Restore{
			"app": {ID: "recover-1", Database: "app", Phase: backup.PhaseReady, Port: 6001, Recover: true},
		},
	}
	for name, view := range map[string]config.Cluster{
		"kept target": cfg.RoutingView(recs.IsAwaiting, "app"),
		"inspection":  cfg.RoutingView(recs.IsAwaiting, ""),
	} {
		spec, port, err := roleVerifierSource(view, recs)
		if err != nil || spec.Name != "app" || port != 6001 {
			t.Fatalf("%s source = %s:%d, %v; want the ready copy app:6001", name, spec.Name, port, err)
		}
	}

	if _, _, err := roleVerifierSource(cfg, backup.Records{AwaitingRecovery: recs.AwaitingRecovery}); err == nil {
		t.Fatal("init's empty pooler view invented a role source")
	}
}

func TestDeferredRecoverFenceSurvivesMissingAwaitingRecord(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Port: 5433}}}

	recs := backup.Records{Restores: map[string]backup.Restore{
		"app": {ID: "id", Database: "app", Phase: backup.PhaseReady, Port: 6433, Recover: true},
	}}
	for _, db := range poolerDatabases(cfg, recs, false) {
		if db.Name == "app" {
			t.Fatal("deferred recovered application was routed before provisioning")
		}
	}

	if got := poolerDatabases(cfg.RoutingView(func(string) bool { return true }, ""), backup.Records{
		AwaitingRecovery: map[string]backup.Awaiting{"app": {}}, Restores: recs.Restores,
	}, true); len(got) != 1 || got[0].Name != backup.RestoringName("app") {
		t.Fatalf("inspection alias lost from awaiting view: %+v", got)
	}
}
