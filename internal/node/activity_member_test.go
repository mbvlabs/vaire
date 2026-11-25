package node

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestMemberActivityIsolation(t *testing.T) { //nolint:gocognit // Exercise the complete pagination and authorization flow.
	store := &activity.Store{Dir: filepath.Join(t.TempDir(), "activity"), MaxBytes: 1 << 20, SegmentBytes: 64 << 10, Retention: time.Hour}
	withActivityStore(t, store)

	for _, actor := range []string{"op-a", "op-b", "op-a"} {
		_, err := store.Append(t.Context(), activity.Event{
			Source: activity.SourceRPC, Action: "status", Outcome: activity.OutcomeSucceeded,
			ActorKind: activity.ActorOperator, ActorVerified: true, OperatorID: actor,
			ActorLabel: "alice", DeviceID: "dev-1", DeviceLabel: "laptop",
			OperationID: testOperationID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err := store.Append(t.Context(), activity.Event{Source: activity.SourceRPC, Action: "status", Outcome: activity.OutcomeSucceeded, ActorKind: activity.ActorOperator, ActorLabel: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	env := activityEnv(t, `{"limit":1}`)
	env.DeviceIdentity = &rpc.WhoamiReply{Role: config.AccessRoleMember, IdentityKind: rpc.IdentityVerifiedDevice, OperatorID: "op-a"}

	res, err := handleActivityList(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}

	page, ok := res.Data.(activity.Page)
	if !ok {
		t.Fatalf("unexpected page: %#v", res.Data)
	}

	if len(page.Events) != 1 || page.Events[0].OperatorID != "op-a" || page.NextCursor == "" {
		t.Fatalf("first page: %+v", page)
	}

	params, _ := json.Marshal(map[string]any{"limit": 1, "cursor": page.NextCursor})
	env.Req.Params = params

	res, err = handleActivityList(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}

	page, ok = res.Data.(activity.Page)
	if !ok {
		t.Fatalf("unexpected page: %#v", res.Data)
	}

	if len(page.Events) != 1 || page.Events[0].OperatorID != "op-a" {
		t.Fatalf("second page: %+v", page)
	}

	env.DeviceIdentity.OperatorID = "op-b"
	if _, err := handleActivityList(t.Context(), env); err == nil {
		t.Fatal("accepted another operator's cursor")
	}

	env.Req.Params = json.RawMessage(`{"operation_id":"` + testOperationID + `"}`)

	res, err = handleActivityShow(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}

	page, ok = res.Data.(activity.Page)
	if !ok {
		t.Fatalf("unexpected page: %#v", res.Data)
	}

	if len(page.Events) != 1 || page.Events[0].OperatorID != "op-b" {
		t.Fatalf("show leaked another operator's events: %+v", page)
	}

	if page.Coverage.Node != "" || page.Coverage.EventsScanned != 0 || page.Coverage.FirstRetainedAt != "" {
		t.Fatalf("show exposed global coverage: %+v", page.Coverage)
	}

	env.Req.Params = json.RawMessage(`{"verifiedOperatorID":"op-a"}`)
	if _, err := handleActivityList(t.Context(), env); err == nil {
		t.Fatal("accepted caller-supplied operator restriction")
	}

	env.Req.Params = json.RawMessage(`{}`)
	if _, err := handleActivityHealth(t.Context(), env); err == nil {
		t.Fatal("member accessed activity health")
	}

	env.DeviceIdentity.IdentityKind = "unverified"
	if _, err := handleActivityList(t.Context(), env); err == nil {
		t.Fatal("unverified member accessed activity")
	}
}
