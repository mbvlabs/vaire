package doctor

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
)

func TestActivationPendingHealthyIsUnchanged(t *testing.T) {
	f := healthy()
	f.Records.Unfollowing = map[string]backup.Unfollow{
		"app": {Source: "source", Settled: true},
	}
	results := Run(NodeChecks(), f, "")
	for _, db := range []string{"app", "other"} {
		if r := byID(results, "activation.pending", db); r.Status != Pass {
			t.Errorf("%s: %s (%s), want pass", db, r.Status, r.Detail)
		}
	}
}

func TestActivationRoutingFenceFails(t *testing.T) {
	f := healthy()
	// An unfollow interrupted after its safety transition leaves a settled
	// record and a routing fence.
	f.Records.Activation = map[string]backup.ActivationFence{
		"app": {OperationID: "unfollow-1", Kind: "unfollow", Database: "app", Phase: "routing"},
	}
	f.Records.Unfollowing = map[string]backup.Unfollow{
		"app": {Source: "source", OperationID: "unfollow-1", Settled: true},
	}
	r := byID(Run(NodeChecks(), f, ""), "activation.pending", "app")
	if r.Status != Fail {
		t.Fatalf("routing fence: %s (%s), want fail", r.Status, r.Detail)
	}
	for _, want := range []string{"routing", "not verified as ready for clients"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not mention %q", r.Detail, want)
		}
	}
	if !strings.Contains(r.Fix, "vaire apply --db app") {
		t.Errorf("fix %q does not finish routing with apply", r.Fix)
	}
	if other := byID(Run(NodeChecks(), f, ""), "activation.pending", "other"); other.Status != Pass {
		t.Errorf("other database inherited the fence: %s (%s)", other.Status, other.Detail)
	}
}

func TestActivationUnfinishedPhaseFails(t *testing.T) {
	f := healthy()
	f.Records.Activation = map[string]backup.ActivationFence{
		"app": {OperationID: "fork-1", Kind: "fork", Database: "app", Phase: "promoted"},
	}
	r := byID(Run(ActivationChecks(), f, ""), "activation.pending", "app")
	if r.Status != Fail || !strings.Contains(r.Detail, "promoted") || !strings.Contains(r.Fix, "fork-1") {
		t.Fatalf("unfinished fence: %+v", r)
	}
}

func TestActivationUnsettledUnfollowFails(t *testing.T) {
	f := healthy()
	f.Records.Unfollowing = map[string]backup.Unfollow{
		"app": {Source: "source", OperationID: "unfollow-1"},
	}
	r := byID(Run(ActivationChecks(), f, ""), "activation.pending", "app")
	if r.Status != Fail || !strings.Contains(r.Detail, "aliases withheld") || !strings.Contains(r.Fix, "vaire db unfollow app") {
		t.Fatalf("unsettled unfollow: %+v", r)
	}
}

func TestActivationPendingFiltersByDatabase(t *testing.T) {
	f := healthy()
	f.Records.Activation = map[string]backup.ActivationFence{
		"app": {OperationID: "restore-1", Kind: "restore-promote", Database: "app", Phase: "routing"},
	}
	for _, r := range Run(ActivationChecks(), f, "other") {
		if r.DB == "app" {
			t.Fatalf("--db other included app's pending fence: %+v", r)
		}
	}
	if r := byID(Run(ActivationChecks(), f, "app"), "activation.pending", "app"); r.Status != Fail {
		t.Errorf("--db app did not report the pending fence: %+v", r)
	}
}

func TestActivationAwaitingRecoverySkips(t *testing.T) {
	f := healthy()
	f.Clusters[0].AwaitingRecovery = true
	if r := byID(Run(ActivationChecks(), f, ""), "activation.pending", "app"); r.Status != Skip {
		t.Errorf("awaiting recovery: %s (%s), want skip", r.Status, r.Detail)
	}
}
