package steps

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
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
