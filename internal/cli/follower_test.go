package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/health"
)

func TestRecoverOrderPutsFollowersLast(t *testing.T) {
	dbs := []databaseInfo{
		{Name: "app_follower", State: stateAwaitingRecovery, Follows: "app", SizeBytes: 1},
		{Name: "big", State: stateAwaitingRecovery, SizeBytes: 100},
		{Name: "app", State: stateAwaitingRecovery, SizeBytes: 50},
	}
	got := recoverOrder(dbs, []string{"app_follower"})
	if !slices.Equal(got, []string{"app", "big", "app_follower"}) {
		t.Fatalf("order %v", got)
	}
}

func TestRecoverFullsSkipFollowers(t *testing.T) {
	outcomes := []dbJobOutcome{
		{Database: "app", Result: map[string]any{"phase": "recovered"}},
		{Database: "app_follower", Result: map[string]any{"follows": "app"}},
	}
	if got := withStanza(outcomes); !slices.Equal(got, []string{"app"}) {
		t.Fatalf("fulls %v", got)
	}
	if line := recoverLine("app_follower", map[string]any{"follows": "app", "from": "stanza"}); !strings.Contains(line, "following app again") {
		t.Fatal(line)
	}
}

func TestFollowerRendering(t *testing.T) {
	st := &health.FollowerState{Known: true, Streaming: true, InRecovery: true, LagBytes: 2048, LagSeconds: 1.4, SlotExists: true, Slot: "vaire_f"}
	if got := followerSuffix(st); got != ", lag 2.0 KiB / 1s" {
		t.Fatalf("suffix %q", got)
	}
	if got := followerSuffix(nil); got != "" {
		t.Fatalf("suffix of a primary %q", got)
	}
	lines := dbListLines([]databaseInfo{{Name: "f", InConfig: true, Follows: "app", State: "following", Follower: st}})
	if !strings.Contains(lines[1], "follows app") || !strings.Contains(lines[1], "following, lag") {
		t.Fatalf("%q", lines[1])
	}
	line := followerLine(statusDB{Name: "f", Follows: "app", Follower: st})
	if !strings.Contains(line, "streaming") || !strings.Contains(line, "slot vaire_f holds") {
		t.Fatal(line)
	}
}
