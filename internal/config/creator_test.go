package config

import (
	"slices"
	"strings"
	"testing"
)

func TestCreatorOwnershipAndRoundTrip(t *testing.T) {
	cfg := DefaultCluster()
	cfg.Clusters = []ClusterSpec{{Name: "app", CreatorOperatorID: "op_123"}, {Name: "legacy"}}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		db, operator string
		want         bool
	}{
		{"app", "op_123", true}, {"app", "other", false},
		{"legacy", "", false}, {"missing", "op_123", false},
	} {
		if got := decoded.DatabaseCreatedByOperator(tc.db, tc.operator); got != tc.want {
			t.Errorf("%s / %s: got %v", tc.db, tc.operator, got)
		}
	}

	if (ClusterSpec{CreatorOperatorID: "bad id"}).CreatedByOperator("bad id") {
		t.Fatal("malformed IDs matched")
	}

	decoded.Clusters = append(decoded.Clusters, decoded.Clusters[0])
	if decoded.DatabaseCreatedByOperator("app", "op_123") {
		t.Fatal("duplicate database matched")
	}
}

func TestCreatorIsServerManaged(t *testing.T) {
	old := DefaultCluster()
	old.Clusters = []ClusterSpec{{Name: "app", CreatorOperatorID: "op_123"}}

	if err := old.Set("clusters.app.creator_operator_id", "op_456"); err == nil {
		t.Fatal("config set accepted creator change")
	}

	for _, id := range []string{"", "op_456"} {
		updated := old.Clone()
		updated.Clusters[0].CreatorOperatorID = id

		if !slices.Contains(ImmutableChanges(old, updated), "clusters.app.creator_operator_id") {
			t.Fatalf("config edit accepted creator %q", id)
		}
	}

	updated := old.Clone()
	updated.Clusters = append(updated.Clusters, ClusterSpec{Name: "new", CreatorOperatorID: "op_456"})

	if !slices.Contains(ImmutableChanges(old, updated), "clusters.new.creator_operator_id") {
		t.Fatal("config edit accepted creator on new database")
	}

	updated.Clusters[0].CreatorOperatorID = "bad id"
	for _, scope := range []Scope{ScopeHost, ScopeFull} {
		if err := updated.ValidateFor(scope); err == nil || !strings.Contains(err.Error(), "creator_operator_id") {
			t.Fatalf("%s: malformed creator accepted: %v", scope, err)
		}
	}
}
