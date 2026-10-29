package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/promfile"
)

func int64p(v int64) *int64       { return &v }
func float64p(v float64) *float64 { return &v }

// followerQueryPG serves the source and self queries separately, so a test
// can fail one side only.
type followerQueryPG struct {
	sourceRows []map[string]string
	sourceErr  error
	selfRows   []map[string]string
	selfErr    error
}

func (f *followerQueryPG) Query(_ context.Context, _, sql string, _ map[string]string) ([]map[string]string, error) {
	if strings.Contains(sql, "pg_stat_replication") || strings.Contains(sql, "pg_replication_slots") {
		return f.sourceRows, f.sourceErr
	}
	return f.selfRows, f.selfErr
}

func (f *followerQueryPG) Exec(context.Context, string, string, map[string]string) error { return nil }

// R4-1: a failed side keeps its own error and stays unknown; it does not
// borrow the other side's answer.
func TestReadFollowerTracksSidesIndependently(t *testing.T) {
	oldRunning, oldPG := clusterRunning, followerPG
	t.Cleanup(func() { clusterRunning, followerPG = oldRunning, oldPG })
	clusterRunning = func(config.ClusterSpec) bool { return true }

	cfg := followerCfg()
	src, follower := cfg.Clusters[0], cfg.Clusters[1]
	srcRow := map[string]string{"sender_state": "streaming", "lag_bytes": "0", "replay_lag": "",
		"slot_active": "true", "slot_retained": "0", "slot_safe": ""}
	selfRow := map[string]string{"in_recovery": "true", "pause_state": "not paused"}

	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceRows: []map[string]string{srcRow}, selfErr: errors.New("connection reset")}
	}
	st := readFollower(context.Background(), follower, src)
	if !st.SourceKnown || st.SelfKnown || !st.Streaming || st.SelfError == "" || st.SourceError != "" {
		t.Fatalf("source read, self failed: %+v", st)
	}

	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceErr: errors.New("statement timeout"), selfRows: []map[string]string{selfRow}}
	}
	st = readFollower(context.Background(), follower, src)
	if st.SourceKnown || !st.SelfKnown || !st.InRecovery || st.SourceError == "" || st.SelfError != "" {
		t.Fatalf("source failed, self read: %+v", st)
	}
}

// R4-5: a failed slot enumeration is returned, not silently read as no
// orphan slots.
func TestOrphanSlotsReportsQueryFailure(t *testing.T) {
	oldRunning, oldPG := clusterRunning, followerPG
	t.Cleanup(func() { clusterRunning, followerPG = oldRunning, oldPG })
	clusterRunning = func(config.ClusterSpec) bool { return true }
	followerPG = func(config.ClusterSpec) sqlRunner {
		return &followerQueryPG{sourceErr: errors.New("canceling statement due to statement timeout")}
	}

	slots, failures := orphanSlots(context.Background(), followerCfg())
	if len(slots) != 0 || len(failures) != 1 || failures[0].Source != "app" ||
		failures[0].Error != "canceling statement due to statement timeout" {
		t.Fatalf("slots %v, failures %v", slots, failures)
	}
}

// R4-6: a follower-only textfile write failure fails refresh instead of
// leaving stale healthy samples behind.
func TestFollowerMetricWriteFailureFailsRefresh(t *testing.T) {
	old := writeFollowerProm
	t.Cleanup(func() { writeFollowerProm = old })
	want := errors.New("textfile directory is not writable")
	writeFollowerProm = func(string, []*promfile.Family) error { return want }

	if err := writeFollowerMetrics(context.Background(), config.Cluster{}); !errors.Is(err, want) {
		t.Fatalf("writeFollowerMetrics: %v", err)
	}
	if err := refreshMetrics(context.Background(), config.Cluster{}); !errors.Is(err, want) {
		t.Fatalf("refreshMetrics lost the follower write error: %v", err)
	}
}
