package node

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

type listingPG struct {
	spec    config.ClusterSpec
	queries *[]string
}

func (p listingPG) Query(_ context.Context, db, sql string, _ map[string]string) ([]map[string]string, error) {
	*p.queries = append(*p.queries, p.spec.Name+"|"+db+"|"+sql)
	if p.spec.Name == "stopped_follower" || p.spec.Name == "pending_follower" {
		return nil, errors.New("unavailable")
	}
	if strings.Contains(sql, "pg_database_size") {
		return []map[string]string{{"size": "123"}}, nil
	}
	return nil, nil
}

func (listingPG) Exec(context.Context, string, string, map[string]string) error { return nil }

func TestDBListStrictInventoryStates(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower", "stopped_follower", "pending_follower", "awaiting_follower")
	oldNames, oldPG, oldFollowerPG, oldRunning, oldRecords := databaseListClusterNames, databaseListPG, followerPG, clusterRunning, backup.RecordsPath
	t.Cleanup(func() {
		databaseListClusterNames, databaseListPG, followerPG, clusterRunning, backup.RecordsPath = oldNames, oldPG, oldFollowerPG, oldRunning, oldRecords
	})
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	if err := backup.UpdateRecords(func(r *backup.Records) error {
		r.AwaitingRecovery["awaiting_follower"] = backup.Awaiting{}
		r.Drilling["app"] = "app__drill"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Strict inventory has already normalized online,recovery/down,recovery.
	databaseListClusterNames = func(context.Context) (map[string]string, error) {
		return map[string]string{"app": "online", "app_follower": "online", "stopped_follower": "down",
			"awaiting_follower": "online", "orphan": "down", "app__drill": "down"}, nil
	}
	var queries []string
	databaseListPG = func(spec config.ClusterSpec) sqlRunner { return listingPG{spec, &queries} }
	clusterRunning = func(config.ClusterSpec) bool { return true }
	followerCalls := 0
	followerPG = func(config.ClusterSpec) sqlRunner {
		followerCalls++
		return &followerQueryPG{
			sourceRows: []map[string]string{{"sender_state": "streaming", "lag_bytes": "0", "slot_active": "true"}},
			selfRows:   []map[string]string{{"in_recovery": "true", "pause_state": "not paused"}},
		}
	}
	res, err := handleDBList(context.Background(), &Env{Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Data.(map[string]any)["databases"].([]databaseInfo)
	byName := make(map[string]databaseInfo)
	for _, row := range rows {
		byName[row.Name] = row
	}
	if len(byName) != 6 || byName["app"].State != stateRunning || byName["orphan"].State != stateOrphaned {
		t.Fatalf("unexpected independent/orphan rows: %+v", rows)
	}
	f := byName["app_follower"]
	if f.State != stateFollowing || f.Follows != "app" || f.Follower == nil || !f.Follower.SourceKnown || !f.Follower.SelfKnown || !f.Follower.InRecovery || !f.Follower.Streaming {
		t.Fatalf("running follower: %+v, facts %+v", f, f.Follower)
	}
	if f.SizeBytes != 123 || !f.Exists {
		t.Fatalf("follower database not read: %+v", f)
	}
	if s := byName["stopped_follower"]; s.State != stateStopped || s.Follower != nil || s.Follows != "app" {
		t.Fatalf("stopped follower: %+v", s)
	}
	if p := byName["pending_follower"]; p.State != statePending || p.Follower != nil {
		t.Fatalf("pending follower: %+v", p)
	}
	if a := byName["awaiting_follower"]; a.State != stateAwaiting || a.Follower != nil {
		t.Fatalf("awaiting follower: %+v", a)
	}
	if followerCalls != 2 {
		t.Fatalf("got %d replication probes, want 2", followerCalls)
	}
	allQueries := strings.Join(queries, "\n")
	if !strings.Contains(allQueries, "app_follower|postgres|SELECT pg_database_size(datname) AS size FROM pg_database WHERE datname = 'app'") ||
		!strings.Contains(allQueries, "app_follower|app|SELECT extname") ||
		strings.Contains(allQueries, "awaiting_follower|") {
		t.Fatalf("follower database identity or awaiting queries: %s", allQueries)
	}
}

func TestDBListInventoryFailureReturnsNoRows(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	old, oldPG := databaseListClusterNames, databaseListPG
	t.Cleanup(func() { databaseListClusterNames, databaseListPG = old, oldPG })
	databaseListPG = func(config.ClusterSpec) sqlRunner {
		t.Fatal("inventory failure must not query databases")
		return nil
	}
	for _, failure := range []string{"command failed", "malformed line", "partial inventory", "unrecognized status"} {
		t.Run(failure, func(t *testing.T) {
			databaseListClusterNames = func(context.Context) (map[string]string, error) {
				return map[string]string{"app": "online"}, errors.New(failure)
			}
			result, err := handleDBList(context.Background(), &Env{Cluster: cfg})
			var cliErr *output.CLIError
			if !errors.As(err, &cliErr) || cliErr.Code != output.CodePrecondition || cliErr.ExitCode != output.ExitPrecondition ||
				cliErr.Details.(map[string]any)["reason"] != "cluster_inventory_unavailable" || result.Data != nil || len(result.Warnings) != 0 {
				t.Fatalf("got result %+v, error %+v", result, err)
			}
		})
	}
}
