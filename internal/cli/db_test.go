package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// db credentials --rotate refuses a follower before protection, the token
// and the new secret, for both a dry run and a real rotation: the preflight
// runs before either path branches (Phase 6, B-04).
func TestCredentialsPreflightRefusesAFollower(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s"},
		{Name: "app_follower", Port: 5434, Size: "small", StatementTimeout: "60s", Follows: "app"},
	}
	for _, protected := range []bool{false, true} {
		cfg.Clusters[1].Protected = protected
		err := credentialsPreflight(cfg, "app_follower")
		var ce *output.CLIError
		if !errors.As(err, &ce) {
			t.Fatalf("credentials on a follower (protected %v): %v, want is_follower", protected, err)
		}
		d, _ := ce.Details.(map[string]any)
		if ce.Code != output.CodePrecondition || d["reason"] != "is_follower" || d["source"] != "app" {
			t.Errorf("credentials on a follower (protected %v): %s %v, want is_follower from app", protected, ce.Code, d)
		}
		if err := credentialsPreflight(cfg, "app"); err != nil {
			t.Errorf("credentials on the source (protected %v): %v", protected, err)
		}
	}
}

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
