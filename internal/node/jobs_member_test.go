package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func memberJobEnv() *Env {
	return &Env{
		DeviceIdentity: &rpc.WhoamiReply{Role: config.AccessRoleMember, IdentityKind: rpc.IdentityVerifiedDevice, OperatorID: "op-a"},
		Report:         func(output.Event) {},
	}
}

func TestMemberFollowSuppressesJournalAfterOverwrite(t *testing.T) {
	oldLoad, oldStream := loadJobState, streamJobJournal

	t.Cleanup(func() { loadJobState, streamJobJournal = oldLoad, oldStream })

	rec := engine.JobRecord{ID: testJobID, Job: "db-copy", Unit: jobUnit(testJobID),
		State: engine.JobSucceeded, OriginOperatorID: "op-a", OriginToken: testOperationID,
		InvocationID: "0123456789abcdef0123456789abcdef"}

	var mu sync.Mutex

	loadJobState = func() (engine.State, error) {
		mu.Lock()
		defer mu.Unlock()

		return engine.State{Jobs: []engine.JobRecord{rec}}, nil
	}
	streamJobJournal = func(_ context.Context, _ []string, report engine.Reporter) {
		mu.Lock()
		rec.OriginToken = testRequestID
		mu.Unlock()
		report(output.Event{Type: "log", Line: "must not escape"})
	}
	env := memberJobEnv()

	var lines []string

	env.Report = func(ev output.Event) { lines = append(lines, ev.Line) }
	if _, err := followJob(t.Context(), env, testJobID); err == nil {
		t.Fatal("follow returned an overwritten job")
	}

	if len(lines) != 0 {
		t.Fatalf("journal escaped after overwrite: %v", lines)
	}
}

func TestMemberJobListAndFollow(t *testing.T) {
	oldLoad, oldStream := loadJobState, streamJobJournal

	t.Cleanup(func() { loadJobState, streamJobJournal = oldLoad, oldStream })

	own := engine.JobRecord{ID: testJobID, Job: "db-copy", Unit: jobUnit(testJobID), State: engine.JobSucceeded,
		OriginOperatorID: "op-a", OriginToken: testOperationID, InvocationID: "0123456789abcdef0123456789abcdef"}
	other := own
	other.ID, other.Unit, other.OriginOperatorID = testRequestID, jobUnit(testRequestID), "op-b"
	legacy := own
	legacy.ID, legacy.Unit, legacy.OriginOperatorID = testParentID, jobUnit(testParentID), ""
	timer := own
	timer.ID, timer.Unit = testOperationID, ""
	state := engine.State{Jobs: []engine.JobRecord{other, legacy, timer, own}}
	loadJobState = func() (engine.State, error) { return state, nil }
	calls := 0
	streamJobJournal = func(_ context.Context, args []string, report engine.Reporter) {
		calls++

		if args[len(args)-1] != "_SYSTEMD_INVOCATION_ID="+own.InvocationID {
			t.Errorf("journal not scoped to invocation: %v", args)
		}

		report(output.Event{Type: "log", Line: "own log"})
	}
	env := memberJobEnv()

	res, err := handleJobList(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}

	jobs, ok := res.Data.(map[string]any)["jobs"].([]engine.JobRecord)
	if !ok || len(jobs) != 1 || jobs[0].ID != own.ID {
		t.Fatalf("member list = %#v", res.Data)
	}

	if _, err := followJob(t.Context(), env, own.ID); err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatalf("journal calls = %d", calls)
	}

	state.Jobs[len(state.Jobs)-1].InvocationID = ""

	if _, err := followJob(t.Context(), env, own.ID); err != nil {
		t.Fatal(err)
	}

	if calls != 1 {
		t.Fatal("follow opened a reusable unit without a verified invocation")
	}

	for _, id := range []string{other.ID, legacy.ID, timer.ID, "01M4X0ME0NBQ95HNNDT213BGHD"} {
		if _, err := followJob(t.Context(), env, id); err == nil {
			t.Fatalf("follow accepted %s", id)
		}
	}

	if calls != 1 {
		t.Fatalf("unauthorized follow opened journal: %d", calls)
	}

	env.DeviceIdentity.IdentityKind = "unverified"
	if _, err := handleJobList(t.Context(), env); err == nil {
		t.Fatal("unverified member listed jobs")
	}
}

func TestJobOriginNotInheritedFromReplacedRecords(t *testing.T) {
	own := engine.JobRecord{ID: testJobID, Job: "db-copy", Unit: jobUnit(testJobID),
		State: engine.JobRunning, OriginOperatorID: "op-a", OriginToken: testOperationID}
	for _, tc := range []struct {
		name    string
		old     engine.JobRecord
		present bool
		want    string
	}{
		{"matching", own, true, "op-a"},
		{"absent", own, false, ""},
		{"other operator", func() engine.JobRecord { r := own; r.OriginOperatorID = "op-b"; return r }(), true, ""},
		{"other attempt", func() engine.JobRecord { r := own; r.OriginToken = testRequestID; return r }(), true, ""},
		{"other job", func() engine.JobRecord { r := own; r.Job = "restore"; return r }(), true, ""},
		{"timer", func() engine.JobRecord { r := own; r.Unit = ""; return r }(), true, ""},
		{"terminal", func() engine.JobRecord { r := own; r.State = engine.JobSucceeded; return r }(), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state engine.State
			if tc.present {
				state.PutJob(tc.old)
			}

			next := own
			clearUnmatchedJobOrigin(&state, &next)

			if next.OriginOperatorID != tc.want {
				t.Fatalf("origin = %q, want %q", next.OriginOperatorID, tc.want)
			}
		})
	}
}

func TestCopyJobSpecAndRecordUseVerifiedOrigin(t *testing.T) {
	oldDir, oldUpdate, oldLaunch := copyJobDir, copyUpdateState, copyLaunchJob

	t.Cleanup(func() { copyJobDir, copyUpdateState, copyLaunchJob = oldDir, oldUpdate, oldLaunch })
	copyJobDir = t.TempDir()

	var state engine.State

	copyUpdateState = func(fn func(*engine.State) error) error { return fn(&state) }
	copyLaunchJob = func(context.Context, *Env, string, string, string) error { return nil }
	env := memberJobEnv()
	env.Req = rpc.Request{RequestID: testJobID, Operator: "spoofed"}

	if _, err := startCopyJob(t.Context(), env, "db-copy", nil, nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(copyJobDir, testJobID+".json"))
	if err != nil {
		t.Fatal(err)
	}

	var spec jobSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	rec, exists := state.Job(testJobID)
	if !exists || rec.OriginOperatorID != "op-a" || spec.OriginOperatorID != "op-a" || rec.OriginToken == "" || rec.OriginToken != spec.OriginToken {
		t.Fatalf("record=%+v spec=%+v", rec, spec)
	}

	if _, err := startCopyJob(t.Context(), env, "db-copy", nil, nil); err == nil {
		t.Fatal("duplicate invocation replaced an unread spec")
	}

	after, err := os.ReadFile(filepath.Join(copyJobDir, testJobID+".json"))
	if err != nil || string(after) != string(raw) {
		t.Fatalf("duplicate changed spec: %v", err)
	}
}
