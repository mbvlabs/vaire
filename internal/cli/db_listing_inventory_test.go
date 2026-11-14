package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/health"
)

func TestDBListHumanAndJSONFollowerStatesAgree(t *testing.T) {
	rows := []databaseInfo{
		{Name: "app", InConfig: true, State: "running"},
		{Name: "app_follower", InConfig: true, State: "following", Follows: "app",
			Follower: &health.FollowerState{SourceKnown: true, SelfKnown: true, Streaming: true, InRecovery: true}},
		{Name: "stopped_follower", InConfig: true, State: "stopped", Follows: "app"},
		{Name: "pending_follower", InConfig: true, State: "pending", Follows: "app"},
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []databaseInfo
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	lines := dbListLines(decoded)
	for i, row := range decoded {
		if !strings.Contains(lines[i+1], row.Name) || !strings.Contains(lines[i+1], row.State) {
			t.Fatalf("human %q disagrees with JSON row %+v", lines[i+1], row)
		}
		if row.Follows != "" && !strings.Contains(lines[i+1], "follows "+row.Follows) {
			t.Fatalf("human %q lost JSON source %+v", lines[i+1], row)
		}
	}
	if decoded[1].Follower == nil || !decoded[1].Follower.SelfKnown || decoded[2].Follower != nil {
		t.Fatalf("replication facts differ: %s", raw)
	}
}
