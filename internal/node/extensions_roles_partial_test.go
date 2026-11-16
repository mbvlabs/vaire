package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

type rolesPartialCase struct {
	name        string
	rolesData   engine.ApplyData
	rolesErr    error
	warnings    []output.Warning
	wantMessage string
}

// TestExtAddWithFollowersRolesFailureUsesBaselinePartialOutcome pins the public
// source-with-followers add path's roles failure contract: an apply error, an
// incomplete roles step (with or without a pending-restart warning), and a
// lingering pending restart all remain the same generic extension_sql_failed
// partial outcome. A warning must not replace the baseline reason or message.
func TestExtAddWithFollowersRolesFailureUsesBaselinePartialOutcome(t *testing.T) {
	rolesID := steps.RolesStepID("app")
	rolesIncomplete := engine.ApplyData{Steps: []engine.ApplyStep{{ID: rolesID, State: engine.StatusIncomplete}}}
	rolesRestartPending := engine.ApplyData{RestartPending: []string{rolesID}}
	const generic = "the extension intent and every cluster's configuration are saved, but creating the SQL failed"

	for _, tc := range []rolesPartialCase{
		{
			name:        "roles apply error",
			rolesErr:    errors.New("roles stopped"),
			wantMessage: generic + ": roles stopped",
		},
		{
			name:        "roles incomplete",
			rolesData:   rolesIncomplete,
			wantMessage: generic,
		},
		{
			name:        "roles incomplete with pending restart warning",
			rolesData:   rolesIncomplete,
			warnings:    []output.Warning{{Code: "extension_pending_restart", Message: "restart pending"}},
			wantMessage: generic,
		},
		{
			name:        "roles pending restart",
			rolesData:   rolesRestartPending,
			wantMessage: generic,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := runFollowerAddRolesPartialCase(t, tc)
			assertFollowerAddRolesPartialOutcome(t, err, tc.wantMessage)
			assertFollowerAddRolesApplyOrder(t, calls)
		})
	}
}

func runFollowerAddRolesPartialCase(t *testing.T, tc rolesPartialCase) ([]engine.Options, error) {
	t.Helper()
	rolesID := steps.RolesStepID("app")
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
	}
	h.applyWarnings = tc.warnings
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if len(opts.Only) == 1 && opts.Only[0] == rolesID {
			return tc.rolesData, tc.rolesErr
		}
		return engine.ApplyData{}, nil
	}
	req := extReq(t, map[string]any{"name": "pg_cron", "db": "app"},
		"app/restart", "app_follower_1/restart", "app_follower_2/restart")
	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	return h.applyCalls, err
}

func assertFollowerAddRolesPartialOutcome(t *testing.T, err error, wantMessage string) {
	t.Helper()
	if err == nil {
		t.Fatal("the roles failure was reported as success")
	}
	if got := extReason(t, err); got != "extension_sql_failed" {
		t.Fatalf("reason = %q, want extension_sql_failed", got)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}
	if cliErr.Message != wantMessage {
		t.Fatalf("message = %q, want %q", cliErr.Message, wantMessage)
	}
	details := extDetails(t, err)
	if details["intent_saved"] != true {
		t.Fatalf("intent_saved = %v, want true", details["intent_saved"])
	}
	completed, _ := details["completed"].([]string)
	if strings.Join(completed, ",") != "app_follower_1,app_follower_2,app" {
		t.Fatalf("completed = %v, want both followers and the source", completed)
	}
	pending, _ := details["pending"].([]string)
	if !slices.Equal(pending, []string{"sql"}) {
		t.Fatalf("pending = %v, want [sql]", pending)
	}
	if details["restart_required"] != true || details["restarted"] != true {
		t.Fatalf("restart fields = required:%v restarted:%v, want true/true",
			details["restart_required"], details["restarted"])
	}
}

func assertFollowerAddRolesApplyOrder(t *testing.T, calls []engine.Options) {
	t.Helper()
	want := []string{
		"app_follower_1:postgres-config-v1@app_follower_1",
		"app_follower_2:postgres-config-v1@app_follower_2",
		"app:postgres-config-v1@app",
		"app:postgres-roles-v1@app",
	}
	if got := extApplyIDs(calls); !slices.Equal(got, want) {
		t.Fatalf("apply calls = %v, want %v", got, want)
	}
}
