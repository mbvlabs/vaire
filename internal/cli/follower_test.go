package cli

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/health"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/steps"
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
	int64p := func(v int64) *int64 { return &v }
	float64p := func(v float64) *float64 { return &v }
	st := &health.FollowerState{SourceKnown: true, SelfKnown: true, Streaming: true, InRecovery: true,
		LagBytes: int64p(2048), LagSeconds: float64p(1.4), SlotExists: true, SlotRetainedBytes: int64p(4096), Slot: "vaire_f"}
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

	// R4-3: a streaming follower whose replay is paused says so; unknown
	// readings and a promoted follower are named, not shown as healthy lag.
	paused := *st
	paused.ReplayPaused = true
	if got := followerSuffix(&paused); got != ", replay PAUSED" {
		t.Fatalf("paused suffix %q", got)
	}
	// A source that measures the lag does not hide an unread follower.
	selfUnknown := *st
	selfUnknown.SelfKnown, selfUnknown.SelfError = false, "connection refused"
	if got := followerSuffix(&selfUnknown); got != ", lag 2.0 KiB / 1s, follower state unknown" {
		t.Fatalf("self-unknown suffix %q", got)
	}
	promoted := *st
	promoted.InRecovery = false
	if got := followerSuffix(&promoted); got != ", out of recovery" {
		t.Fatalf("promoted suffix %q", got)
	}
	unknown := health.FollowerState{Source: "app", Slot: "vaire_f"}
	if got := followerSuffix(&unknown); got != ", replication unknown" {
		t.Fatalf("unknown suffix %q", got)
	}
	if got := followerLine(statusDB{Name: "f", Follows: "app", Follower: &unknown}); !strings.Contains(got, "replication unknown") {
		t.Fatalf("unknown line %q", got)
	}

	// R4-4: an unknown measurement is not printed as zero.
	noSeconds := *st
	noSeconds.LagSeconds = nil
	if got := followerSuffix(&noSeconds); !strings.Contains(got, "delay unknown") {
		t.Fatalf("unknown seconds printed as zero: %q", got)
	}
}

// R2-5: a db unfollow that failed after it may have changed the roles hands the
// generated credentials to the operator, and a refusal the node answered before
// it started hands over nothing. The failure itself, its details and the job
// history never carry a password.
func TestUnfollowCredentialsOnFailure(t *testing.T) {
	secret, _, err := newDatabaseSecret(config.Cluster{Domain: "example.com"}, "app_follower", steps.DatabaseRoles)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		err      error
		dryRun   bool
		handover bool
	}{
		"node marked it": {
			err:      output.NewError(output.CodeError, "job 01J failed", output.ExitGeneric, "").WithDetails(map[string]any{node.UnfollowPasswordsDetail: true}),
			handover: true,
		},
		"connection lost": {
			err:      output.NewError(output.CodeTransportFailed, "the connection to the node was lost", output.ExitTransport, ""),
			handover: true,
		},
		"job died without a result": {
			err:      output.NewError(output.CodeError, "job 01J ended without recording a result", output.ExitGeneric, "").WithDetails(map[string]any{"reason": node.JobOutcomeUnknown}),
			handover: true,
		},
		"dry run lost the connection": {
			err:    output.NewError(output.CodeTransportFailed, "the connection to the node was lost", output.ExitTransport, ""),
			dryRun: true,
		},
		"job skipped for the lock": {
			err: output.NewError(output.CodePrecondition, "job 01J was skipped: the node lock was busy", output.ExitPrecondition, "").
				WithDetails(map[string]any{"reason": "lock_held", "job_id": "01J"}),
		},
		"refused before it started": {
			err: output.NewError(output.CodeInputRequired, "the password for app_follower_app was not supplied", output.ExitInput, "").
				WithDetails(map[string]any{"secret": "app_follower_app"}), //nolint:gosec // G101: a secret's name, not its value.
		},
		"dry run": {
			err: output.NewError(output.CodePrecondition, "app_follower follows nothing, so there is nothing to unfollow", output.ExitPrecondition, "").
				WithDetails(map[string]any{"reason": "not_a_follower", "database": "app_follower"}),
		},
	} {
		note, out := unfollowCredentials(test.err, "app_follower", secret, test.dryRun)
		if (note != "") != test.handover {
			t.Errorf("%s: note %q, want a handover %v", name, note, test.handover)
			continue
		}
		raw, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if !test.handover {
			if string(raw) != mustJSON(t, test.err) {
				t.Errorf("%s: a failure without a handover changed the error: %s", name, raw)
			}
			continue
		}
		if !strings.Contains(note, "may have changed app_follower's roles and passwords") {
			t.Errorf("%s: note %q", name, note)
		}
		var cliErr *output.CLIError
		if !errors.As(out, &cliErr) {
			t.Fatalf("%s: not a CLI error: %v", name, out)
		}
		details, _ := cliErr.Details.(map[string]any)
		shown, _ := details["database"].(databaseSecret)
		if shown.Roles["app"].Password != secret.Roles["app"].Password {
			t.Errorf("%s: the handover carries %v", name, details["database"])
		}
		if cliErr.Error() != test.err.Error() {
			t.Errorf("%s: the failure's message changed: %q", name, cliErr.Error())
		}
		var want *output.CLIError
		if errors.As(test.err, &want) && cliErr.Code != want.Code {
			t.Errorf("%s: the failure's code changed to %q", name, cliErr.Code)
		}
		for _, role := range secret.Roles {
			if strings.Contains(cliErr.Error(), role.Password) {
				t.Errorf("%s: the failure's message carries a password", name)
			}
		}
	}
}

// R2-5: the CLI consumes the failure body the node serializes for a job whose
// unit died without recording a result: the reason survives the RPC JSON, and
// the passwords are added only to the explicit credential handoff.
func TestUnfollowCredentialsAfterCrashedJobEnvelope(t *testing.T) {
	raw := []byte(`{"schema":"vaire/v1","ok":false,"error":{"code":"error","message":"job 01J ended without recording a result","hint":"Read journalctl -u vaire-job-01J.service on the node.","details":{"reason":"job_outcome_unknown","job":"db-unfollow","job_id":"01J"}}}`)
	var failure struct {
		Error output.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(raw, &failure); err != nil {
		t.Fatal(err)
	}
	// This is the CLI error the remote result parser builds from that body.
	err := &output.CLIError{Code: failure.Error.Code, Message: failure.Error.Message, Hint: failure.Error.Hint,
		ExitCode: output.ExitFor(failure.Error.Code), Details: failure.Error.Details}
	secret, _, err2 := newDatabaseSecret(config.Cluster{Domain: "example.com"}, "app_follower", steps.DatabaseRoles)
	if err2 != nil {
		t.Fatal(err2)
	}
	note, out := unfollowCredentials(err, "app_follower", secret, false)
	if note == "" {
		t.Fatalf("the crashed job handed over no credentials: %v", out)
	}
	var cliErr *output.CLIError
	if !errors.As(out, &cliErr) {
		t.Fatalf("not a CLI error: %v", out)
	}
	details, _ := cliErr.Details.(map[string]any)
	shown, _ := details["database"].(databaseSecret)
	if shown.Roles["app"].Password != secret.Roles["app"].Password {
		t.Fatal("the handoff does not carry the generated passwords")
	}
	for _, role := range secret.Roles {
		if strings.Contains(string(raw), role.Password) {
			t.Fatal("the node's failure body carries a password")
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
