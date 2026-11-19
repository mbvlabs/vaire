package steps

import (
	"context"
	"errors"
	"os"
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

	files := pgbouncerFilesFrom(view, engine.Runtime{Host: hostinfo.Info{VCPU: 2}}, recs, nil)
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

// publishFollowerTestFixture is one recovering follower, one unrelated
// database still awaiting recovery, and one unrelated unfinished follower. The
// target and the unrelated awaiting database are in the durable records.
func publishFollowerTestFixture(t *testing.T) (config.Cluster, map[string]string) {
	t.Helper()

	oldPath, oldInstance := backup.RecordsPath, poolerInstance
	oldWrite, oldReload := writePoolerFile, reloadPoolers
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	poolerInstance = func() (string, error) { return "test-instance", nil }

	cfg := followerConfig()

	cfg.Clusters = append(cfg.Clusters,
		config.ClusterSpec{Name: "app_pending", Node: "db1", Port: 5435, Size: "small", Follows: "app", FollowGeneration: "0123456789abcdef0123456789abcdef"},
		config.ClusterSpec{Name: "other", Node: "db1", Port: 5436, Size: "small", Stanza: "other-abc123"},
	)
	for i := range cfg.Clusters {
		cfg.Clusters[i].StatementTimeout = "60s"
	}

	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery["app_follower"] = backup.Awaiting{Stanza: ""}
		s.AwaitingRecovery["other"] = backup.Awaiting{Stanza: "other-abc123"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	written := map[string]string{}
	writePoolerFile = func(path, content string, _ os.FileMode, _, _ string) error {
		written[path] = content

		return nil
	}
	reloadPoolers = func(context.Context, engine.Runtime, engine.Reporter) error { return nil }

	t.Cleanup(func() {
		backup.RecordsPath, poolerInstance = oldPath, oldInstance
		writePoolerFile, reloadPoolers = oldWrite, oldReload
	})

	return cfg, written
}

// assertPublishedFollowerRoutes checks both poolers name the recovered
// follower's application and __auth routes while the unrelated unfinished and
// still-awaiting databases stay withheld.
func assertPublishedFollowerRoutes(t *testing.T, written map[string]string) {
	t.Helper()

	for _, path := range []string{poolerFilePaths.ini, poolerFilePaths.sessionIni} {
		content, ok := written[path]
		if !ok {
			t.Fatalf("the follower recovery did not render %s", path)
		}

		for _, want := range []string{
			"app_follower = host=/var/run/postgresql port=5434 dbname=app auth_dbname=app_follower__auth",
			"app_follower__auth = host=/var/run/postgresql port=5434 dbname=postgres",
			"app = host=/var/run/postgresql port=5433",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s lacks %q:\n%s", path, want, content)
			}
		}

		for _, withheld := range []string{"app_pending =", "app_pending__auth =", "other =", "other__auth ="} {
			if strings.Contains(content, withheld) {
				t.Errorf("%s published the withheld %q:\n%s", path, withheld, content)
			}
		}
	}
}

func TestPublishFollowerRoutesPublishesOnlyTheRecoveringTarget(t *testing.T) {
	cfg, written := publishFollowerTestFixture(t)

	if err := publishFollowerRoutesGuarded(context.Background(), cfg, engine.Runtime{Host: followerHost}, "app_follower", nil); err != nil {
		t.Fatalf("the follower route publish failed: %v", err)
	}

	assertPublishedFollowerRoutes(t, written)

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if !recs.IsAwaiting("app_follower") {
		t.Fatal("the route publish cleared the target's awaiting record")
	}
}

// The single reload call covers both poolers. A second-pooler failure is
// therefore a non-nil reload result after both files are already written; the
// durable records must still leave the recovery retryable.
func TestPublishFollowerRoutesSecondPoolerReloadFailure(t *testing.T) {
	cfg, written := publishFollowerTestFixture(t)
	reloadPoolers = func(context.Context, engine.Runtime, engine.Reporter) error {
		return errors.New("vaire-pgbouncer-session.service: Job for vaire-pgbouncer-session.service failed")
	}

	err := publishFollowerRoutesGuarded(context.Background(), cfg, engine.Runtime{Host: followerHost}, "app_follower", nil)
	if err == nil || !strings.Contains(err.Error(), "vaire-pgbouncer-session") {
		t.Fatalf("the second pooler's reload failure was not returned: %v", err)
	}

	assertPublishedFollowerRoutes(t, written)

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if !recs.IsAwaiting("app_follower") {
		t.Fatal("a failed reload cleared the target's awaiting record")
	}
}

type publishRefusalCase struct {
	add        func(*backup.Records)
	wantReason string
	check      func(*testing.T, backup.Records)
}

func TestPublishFollowerRoutesRefusesFencedOrInvalidRecords(t *testing.T) {
	for name, test := range map[string]publishRefusalCase{
		"target activation fence": {
			add: func(s *backup.Records) {
				s.Activation["app_follower"] = backup.ActivationFence{OperationID: "op", Kind: "unfollow", Database: "app_follower", Phase: "prepared"}
			},
			wantReason: "routing_fenced",
			check: func(t *testing.T, recs backup.Records) {
				if _, ok := recs.ActivationFenceFor("app_follower"); !ok {
					t.Fatal("the refusal removed the target's activation fence")
				}
			},
		},
		"target restore fence": {
			add: func(s *backup.Records) {
				s.Restores["app_follower"] = backup.Restore{ID: "rec", Database: "app_follower", Phase: backup.PhaseReady, Port: 6000, Recover: true}
			},
			wantReason: "routing_fenced",
			check: func(t *testing.T, recs backup.Records) {
				if r, ok := recs.Restores["app_follower"]; !ok || !r.Recover {
					t.Fatal("the refusal removed the target's restore fence")
				}
			},
		},
		"unrelated invalid routing fence": {
			add: func(s *backup.Records) {
				s.Activation["other"] = backup.ActivationFence{
					OperationID: "op", Kind: "fork", Database: "other", Instance: "test-instance",
					TargetStanza: "other-abc123", OldStanza: "other-abc123", NewStanza: "other-abc123",
					Port: 9999, Phase: "routing",
				}
			},
			wantReason: "routing_eligibility",
			check: func(t *testing.T, recs backup.Records) {
				if f, ok := recs.ActivationFenceFor("other"); !ok || f.Phase != "routing" {
					t.Fatal("the refusal changed the unrelated routing fence")
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) { runPublishRefusalCase(t, test) })
	}
}

func runPublishRefusalCase(t *testing.T, test publishRefusalCase) {
	t.Helper()

	cfg, written := publishFollowerTestFixture(t)
	if err := backup.UpdateRecords(func(s *backup.Records) error { test.add(s); return nil }); err != nil {
		t.Fatal(err)
	}

	reloaded := false
	reloadPoolers = func(context.Context, engine.Runtime, engine.Reporter) error {
		reloaded = true
		return nil
	}

	err := publishFollowerRoutesGuarded(context.Background(), cfg, engine.Runtime{Host: followerHost}, "app_follower", nil)
	if err == nil || refusalReason(err) != test.wantReason {
		t.Fatalf("refusal = %v, want reason %s", err, test.wantReason)
	}

	if reloaded {
		t.Fatal("a refused publish reloaded the poolers")
	}

	if len(written) != 0 {
		t.Fatalf("a refused publish wrote %d pooler files", len(written))
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if !recs.IsAwaiting("app_follower") {
		t.Fatal("a refused publish cleared the target's awaiting record")
	}

	test.check(t, recs)
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
