package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
)

// A resize preview names the followers first, whether their floors change or
// their restart is already pending (Phase 6, Q1a, R1-3).
func TestResizePreviewMessageShowsTheFollowers(t *testing.T) {
	source := engine.PlanData{WouldApply: 3, Total: 9}
	follower := engine.PlanData{WouldApply: 1, Total: 1}
	followers := []resizeFollowerPlan{{Follower: "app_follower", Restart: engine.RestartRestart, Plan: follower}}
	got := resizePreviewMessage("app", "medium, 1 GiB", source, followers)
	if !strings.Contains(got, "the followers first: app_follower (1 of 1 steps) and restarts; then app (3 of 9 steps)") {
		t.Errorf("preview %q", got)
	}
	// An already pending restart counts as restarting even without a floor
	// change, or without a step left to apply.
	pending := []resizeFollowerPlan{{Follower: "app_follower", Restart: engine.RestartRestart, Plan: engine.PlanData{Total: 1}}}
	got = resizePreviewMessage("app", "medium, 1 GiB", source, pending)
	if !strings.Contains(got, "app_follower (0 of 1 steps) and restarts") {
		t.Errorf("pending restart preview %q", got)
	}
	// Without followers the preview is the source's alone.
	if got := resizePreviewMessage("app", "medium, 1 GiB", source, nil); !strings.Contains(got, "3 of 9 steps would apply") || strings.Contains(got, "followers") {
		t.Errorf("no followers preview %q", got)
	}
}
