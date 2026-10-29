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

// Q3/R4-1: db list and status carry a follower's state as it was read. A fact
// whose side did not answer is left out of the JSON instead of being encoded
// as false or zero, and a measured zero is kept.
func TestFollowerJSONOmitsUnreadSides(t *testing.T) {
	int64p := func(v int64) *int64 { return &v }
	float64p := func(v float64) *float64 { return &v }
	cases := map[string]struct {
		state   health.FollowerState
		present []string
		absent  []string
	}{
		"both sides known": {
			state: health.FollowerState{Source: "app", Slot: "vaire_f", SourceKnown: true, SelfKnown: true,
				Streaming: true, SenderState: "streaming", InRecovery: true,
				LagBytes: int64p(0), LagSeconds: float64p(0), SlotExists: true, SlotActive: true,
				SlotWALStatus: "reserved", SlotRetainedBytes: int64p(0), SlotSafeBytes: 1024},
			present: []string{"source_known", "self_known", "streaming", "sender_state", "in_recovery", "replay_paused",
				"lag_bytes", "lag_seconds", "slot_exists", "slot_active", "slot_wal_status", "slot_retained_bytes", "slot_safe_bytes"},
			absent: []string{"source_error", "self_error"},
		},
		"source only": {
			state: health.FollowerState{Source: "app", Slot: "vaire_f", SourceKnown: true,
				SelfError: "app_follower: connection refused", SlotExists: true, SlotWALStatus: "extended",
				SlotRetainedBytes: int64p(3000), SlotSafeBytes: 1000},
			present: []string{"source_known", "self_known", "streaming", "slot_exists", "slot_active",
				"slot_wal_status", "slot_retained_bytes", "slot_safe_bytes", "self_error"},
			absent: []string{"source_error", "in_recovery", "replay_paused", "lag_bytes", "lag_seconds"},
		},
		"self only": {
			state: health.FollowerState{Source: "app", Slot: "vaire_f", SelfKnown: true,
				SourceError: "app: connection refused", InRecovery: true, ReplayPaused: true},
			present: []string{"source_known", "self_known", "source_error", "in_recovery", "replay_paused"},
			absent: []string{"self_error", "streaming", "slot_exists", "slot_active", "slot_wal_status",
				"slot_safe_bytes", "lag_bytes", "lag_seconds", "slot_retained_bytes"},
		},
		"neither side known": {
			state:   health.FollowerState{Source: "app", Slot: "vaire_f", SourceError: "app: stopped", SelfError: "app_follower: stopped"},
			present: []string{"source_known", "self_known", "source_error", "self_error"},
			absent: []string{"streaming", "sender_state", "in_recovery", "replay_paused", "lag_bytes", "lag_seconds",
				"slot_exists", "slot_active", "slot_wal_status", "slot_retained_bytes", "slot_safe_bytes"},
		},
	}
	rows := map[string]func(*health.FollowerState) any{
		"db list": func(st *health.FollowerState) any {
			return databaseInfo{Name: "app_follower", InConfig: true, State: "following", Follows: "app", Follower: st}
		},
		"status": func(st *health.FollowerState) any {
			return statusData{Databases: []statusDB{{Name: "app_follower", State: "following", Follows: "app", Follower: st}}}
		},
	}
	for rowName, encode := range rows {
		t.Run(rowName, func(t *testing.T) {
			for name, tc := range cases {
				st := tc.state
				f := followerField(t, encode(&st))
				if got, _ := f["source_known"].(bool); got != st.SourceKnown {
					t.Errorf("%s: source_known %v, want %v", name, f["source_known"], st.SourceKnown)
				}
				if got, _ := f["self_known"].(bool); got != st.SelfKnown {
					t.Errorf("%s: self_known %v, want %v", name, f["self_known"], st.SelfKnown)
				}
				for _, key := range tc.present {
					if _, ok := f[key]; !ok {
						t.Errorf("%s: %s is missing from %v", name, key, f)
					}
				}
				for _, key := range tc.absent {
					if _, ok := f[key]; ok {
						t.Errorf("%s: %s is in JSON though its side is unread: %v", name, key, f)
					}
				}
			}
			// A measured zero is not an unknown: both sides answered, so the
			// caught-up follower keeps its zeros.
			st := cases["both sides known"].state
			f := followerField(t, encode(&st))
			for _, key := range []string{"lag_bytes", "lag_seconds", "slot_retained_bytes"} {
				if got, ok := f[key].(float64); !ok || got != 0 {
					t.Errorf("a measured zero was lost: %s = %v", key, f[key])
				}
			}
		})
	}
}

// followerField is the follower object inside a db list or status row's JSON.
func followerField(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	f, ok := doc["follower"].(map[string]any)
	if !ok {
		// status nests its rows under databases.
		rows, _ := doc["databases"].([]any)
		if len(rows) == 1 {
			row, _ := rows[0].(map[string]any)
			f, ok = row["follower"].(map[string]any)
		}
	}
	if !ok {
		t.Fatalf("no follower object in %s", raw)
	}
	return f
}
