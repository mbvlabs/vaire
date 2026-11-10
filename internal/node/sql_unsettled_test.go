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
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the fake CLI must be executable.
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
			if _, err := sqlRead(node.config(), "app_follower", "SELECT 1"); err != nil {
				t.Fatalf("an ordinary follower read is refused: %v", err)
			}
			if !ran() {
				t.Fatal("an ordinary follower read did not reach psql")
			}
			clear()

			stub := (&followerStub{failAt: stop, failErr: errors.New("stopped at " + stop)}).install(t)
			if _, err := jobUnfollow(ctx, unfollowEnv(node.config(), "app_follower")); err == nil {
				t.Fatalf("the unfollow that stopped at %s returned no error", stop)
			}
			if !stub.reached("promote") {
				t.Fatalf("the unfollow stopped at %s before the promotion", stop)
			}
			if stop == "rename" && !stub.reached("settle") {
				t.Fatal("the rename failure did not follow the first policy settlement")
			}
			if stub.resumed {
				t.Fatal("the failed unfollow resumed the poolers before settlement")
			}
			recs := node.records()
			unfinished, ok := recs.Unfollowing["app_follower"]
			if !ok || unfinished.Settled {
				t.Fatalf("the failed unfollow left no unsettled record: %+v", unfinished)
			}

			if _, err := sqlRead(node.config(), "app_follower", escape); err == nil {
				t.Fatal("the write:false transaction escape ran through the unsettled unfollow")
			} else if got := reason(t, err); got != "unfollow_unsettled" {
				t.Fatalf("refusal reason = %q, want unfollow_unsettled", got)
			}
			if ran() {
				t.Fatal("psql ran for the refused transaction escape")
			}

			stub.failAt = ""
			if _, err := jobUnfollow(ctx, unfollowEnv(node.config(), "app_follower")); err != nil {
				t.Fatalf("the resumed unfollow failed: %v", err)
			}
			if !stub.resumed {
				t.Fatal("the finished unfollow did not resume the poolers")
			}
			if recs := node.records().Unfollowing; len(recs) != 0 {
				t.Fatalf("the finished unfollow left a record: %v", recs)
			}
			clear()
			if _, err := sqlRead(node.config(), "app_follower", "SELECT 1"); err != nil {
				t.Fatalf("a settled read is refused: %v", err)
			}
			if !ran() {
				t.Fatal("a settled read did not reach psql")
			}
		})
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
	if err := os.WriteFile(backup.RecordsPath, []byte("{"), 0o600); err != nil { //nolint:gosec // G306: test records file.
		t.Fatal(err)
	}
	if _, err := sqlRead(cfg, "app_follower", "SELECT 1"); err == nil {
		t.Fatal("SQL ran with unreadable unfollow state")
	} else if got := reason(t, err); got != "unfollow_state_unknown" {
		t.Fatalf("refusal reason = %q, want unfollow_state_unknown", got)
	}
	if ran() {
		t.Fatal("psql ran with unreadable unfollow state")
	}
}

// A protected primary has no unfinished unfollow: its read path keeps running
// as vaire_admin, and its write gate stays behind the authenticator app.
func TestSQLReadOnAProtectedPrimaryIsUnchanged(t *testing.T) {
	cfg := protectedCluster()
	ran, _ := fakeSQLBinaries(t)
	if _, err := sqlRead(cfg, "app", "SELECT 1"); err != nil {
		t.Fatalf("a protected primary read is refused: %v", err)
	}
	if !ran() {
		t.Fatal("a protected primary read did not reach psql")
	}
}
