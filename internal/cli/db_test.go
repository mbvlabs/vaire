package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/remote"
)

func TestDBCreateDryRunNodePreview(t *testing.T) {
	db := config.ClusterSpec{Name: "app", Size: "small", Memory: "768MiB", Profile: "oltp", StatementTimeout: "30s", Extensions: []string{"pg_stat_statements"}, Protected: true}
	calls := 0
	planned, warnings, err := previewDBCreate(db, true, func(method string, params map[string]any, opts remote.CallOptions) (remote.Reply, error) {
		calls++
		if method != "db.create" || !opts.DryRun || len(opts.Secrets) != 0 {
			t.Fatalf("method %q opts %+v", method, opts)
		}
		for key, want := range map[string]any{
			"name": "app", "size": "small", "memory": "768MiB", "profile": "oltp", "statement_timeout": "30s",
			"protected": true, "allow_overcommit": true,
		} {
			if params[key] != want {
				t.Errorf("%s = %v, want %v", key, params[key], want)
			}
		}
		extensions, ok := params["extensions"].([]string)
		if !ok || len(extensions) != 1 || extensions[0] != "pg_stat_statements" {
			t.Errorf("extensions: %v", params["extensions"])
		}
		return remote.Reply{
			Data:     []byte(`{"database":{"name":"app","size":"medium","statement_timeout":"30s"}}`),
			Warnings: []output.Warning{{Code: "memory_overcommitted"}},
		}, nil
	})
	if err != nil || calls != 1 || planned.Size != "medium" || len(warnings) != 1 || warnings[0].Code != "memory_overcommitted" {
		t.Fatalf("preview %+v warnings %+v calls %d err %v", planned, warnings, calls, err)
	}
}

func TestDBCreateDryRunRefusalAndInvalidReply(t *testing.T) {
	refusal := output.NewError(output.CodePrecondition, "unsafe topology", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": "follower_recovery_floor_decrease"})
	_, _, err := previewDBCreate(config.ClusterSpec{Name: "app"}, false, func(_ string, _ map[string]any, opts remote.CallOptions) (remote.Reply, error) {
		if !opts.DryRun || len(opts.Secrets) != 0 {
			t.Fatal("preview sent secrets or was not dry run")
		}
		return remote.Reply{}, refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("refusal lost: %v", err)
	}
	_, _, err = previewDBCreate(config.ClusterSpec{Name: "app"}, false, func(_ string, _ map[string]any, _ remote.CallOptions) (remote.Reply, error) {
		return remote.Reply{Data: []byte(`{}`)}, nil
	})
	if err == nil {
		t.Fatal("missing planned database reported success")
	}
}

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
