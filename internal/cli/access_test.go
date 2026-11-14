package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestAccessMessages(t *testing.T) {
	msg := accessMessage(databaseAccess{
		Database: "replica", Desired: "automatic", Source: "app",
		Owner: "app_owner", App: "app_app", RO: "app_ro",
		Coverage: steps.AccessCoverage{Status: "pending", SchemaMissing: 1, UncoveredCreators: []string{"other"}},
		Notice:   "global defaults",
	})
	for _, want := range []string{"coverage pending", "inherited from: app", "schemas 1", "uncovered creators: other", "global defaults"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q: %s", want, msg)
		}
	}
	unknown := accessMessage(databaseAccess{Coverage: steps.AccessCoverage{Status: "unknown"}})
	if strings.Contains(unknown, "missing:") {
		t.Fatalf("unknown printed zero counts as measurements: %s", unknown)
	}
	plan := planMessage("plan", engine.PlanData{
		Steps:               []engine.PlanStep{{Action: "apply", ID: "postgres-access-v1@app", Detail: "global owner defaults"}},
		AccessConfirmTokens: []string{"app/access"},
	})
	if !strings.Contains(plan, "--confirm app/access") || !strings.Contains(plan, "global owner defaults") {
		t.Fatalf("plan lost access scope: %s", plan)
	}
}
