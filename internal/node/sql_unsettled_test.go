package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
)

// fakeSQLBinaries puts a psql and a runuser on PATH that record any SQL
// invocation in marker. The handlers under test must refuse before either
// runs; a read that is allowed reaches psql.
func fakeSQLBinaries(t *testing.T) (ran func() bool, clear func()) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "sql-ran")
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$*\" >> \"" + marker + "\"\nexit 0\n"
	for _, name := range []string{"psql", "runuser"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return func() bool {
			_, err := os.Stat(marker)
			return err == nil
		}, func() {
			_ = os.Remove(marker)
		}
}

func sqlRead(cfg config.Cluster, db, query string) (Result, error) {
	req := request("sql", map[string]any{"db": db, "query": query})
	return methods()["sql"].handler(context.Background(), &Env{Cluster: cfg, Req: req})
}

// A-R4 / RB-03: a protected follower copied from an unprotected source keeps
// vaire_admin's owner membership. An unfollow promotes it, and a failure
// before the renames and the protection settlement leave it configured as a
// follower but writable. A write:false query can COMMIT out of the read-only
// transaction and write as the inherited owner, so the real SQL wrapper must
// refuse every statement through the unsettled transition, before psql runs.
// The same test proves an ordinary follower read outside the transition and a
// settled read still run.
func TestSQLReadEscapeRefusedForProtectedUnsettledUnfollow(t *testing.T) {
	const escape = "COMMIT;\nSET ROLE app_owner;\nDELETE FROM t;\nCOMMIT;"
	for _, stop := range []string{"settle", "rename"} {
		t.Run(stop, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			cfg.Clusters[1].Protected = true
			node := newFollowerNode(t, cfg)
			ran, clear := fakeSQLBinaries(t)

			// Outside any transition a protected follower read is ordinary
			// and still reaches PostgreSQL.
			requireFollowerReadReachesPSQL(t, node.config(), ran, "an ordinary follower read")
			clear()

			stub := (&followerStub{failAt: stop, failErr: errors.New("stopped at " + stop)}).install(t)
			requireUnsettledUnfollowStop(t, ctx, node, stub, stop)
			requireUnsettledSQLRefusal(t, node.config(), escape, ran)

			stub.failAt = ""
			requireUnfollowSettlement(t, ctx, node, stub)
			clear()
			requireFollowerReadReachesPSQL(t, node.config(), ran, "a settled read")
		})
	}
}

// requireFollowerReadReachesPSQL asserts label is allowed through and reaches
// the fake psql binary.
func requireFollowerReadReachesPSQL(t *testing.T, cfg config.Cluster, ran func() bool, label string) {
	t.Helper()
	if _, err := sqlRead(cfg, "app_follower", "SELECT 1"); err != nil {
		t.Fatalf("%s is refused: %v", label, err)
	}
	rejectf(t, !ran(), "%s did not reach psql", label)
}

// requireUnsettledUnfollowStop asserts the unfollow stopped at stop after the
// promotion, leaving its record unsettled and the poolers down.
func requireUnsettledUnfollowStop(t *testing.T, ctx context.Context, node *followerNode, stub *followerStub, stop string) {
	t.Helper()
	if _, err := jobUnfollow(ctx, unfollowEnv(node.config(), "app_follower")); err == nil {
		t.Fatalf("the unfollow that stopped at %s returned no error", stop)
	}
	rejectf(t, !stub.reached("promote"), "the unfollow stopped at %s before the promotion", stop)
	reject(t, stop == "rename" && !stub.reached("settle"), "the rename failure did not follow the first policy settlement")
	reject(t, stub.resumed, "the failed unfollow resumed the poolers before settlement")
	recs := node.records()
	unfinished, ok := recs.Unfollowing["app_follower"]
	rejectf(t, !ok || unfinished.Settled, "the failed unfollow left no unsettled record: %+v", unfinished)
}

// requireUnsettledSQLRefusal asserts the escape is refused before psql runs.
func requireUnsettledSQLRefusal(t *testing.T, cfg config.Cluster, escape string, ran func() bool) {
	t.Helper()
	if _, err := sqlRead(cfg, "app_follower", escape); err == nil {
		t.Fatal("the write:false transaction escape ran through the unsettled unfollow")
	} else if got := reason(t, err); got != "unfollow_unsettled" {
		t.Fatalf("refusal reason = %q, want unfollow_unsettled", got)
	}
	reject(t, ran(), "psql ran for the refused transaction escape")
}

// requireUnfollowSettlement asserts a resumed unfollow finishes, resumes the
// poolers and clears its record.
func requireUnfollowSettlement(t *testing.T, ctx context.Context, node *followerNode, stub *followerStub) {
	t.Helper()
	if _, err := jobUnfollow(ctx, unfollowEnv(node.config(), "app_follower")); err != nil {
		t.Fatalf("the resumed unfollow failed: %v", err)
	}
	reject(t, !stub.resumed, "the finished unfollow did not resume the poolers")
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Fatalf("the finished unfollow left a record: %v", recs)
	}
}

// A protected database whose transition records cannot be read is unknown
// state: refuse before psql rather than assume there is no unfinished
// unfollow.
func TestSQLRefusedOnProtectedDatabaseWhenUnfollowStateIsUnreadable(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[1].Protected = true
	newFollowerNode(t, cfg)
	ran, _ := fakeSQLBinaries(t)
	if err := os.WriteFile(backup.RecordsPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlRead(cfg, "app_follower", "SELECT 1"); err == nil {
		t.Fatal("SQL ran with unreadable unfollow state")
	} else if got := reason(t, err); got != "unfollow_state_unknown" {
		t.Fatalf("refusal reason = %q, want unfollow_state_unknown", got)
	}
	reject(t, ran(), "psql ran with unreadable unfollow state")
}

// A protected primary has no unfinished unfollow: its read path keeps running
// as vaire_admin, and its write gate stays behind the authenticator app.
func TestSQLReadOnAProtectedPrimaryIsUnchanged(t *testing.T) {
	cfg := protectedCluster()
	ran, _ := fakeSQLBinaries(t)
	if _, err := sqlRead(cfg, "app", "SELECT 1"); err != nil {
		t.Fatalf("a protected primary read is refused: %v", err)
	}
	reject(t, !ran(), "a protected primary read did not reach psql")
}
