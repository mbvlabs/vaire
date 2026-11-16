package node

import (
	"context"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
)

func TestBuildCopyPlanPreservesManagedPreloadOrder(t *testing.T) {
	cfg := planCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_kcache": {
			Source: catalog.SourcePreinstalled, ExtName: "pg_stat_kcache", Preload: "pg_stat_kcache",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"pg_stat_kcache"}
	spec := cfg.Clusters[0]
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, Stanza: spec.Stanza, CopyDB: "app", Standby: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache"}
	if !slices.Equal(plan.Plan.DesiredPreloads, want) {
		t.Fatalf("copy desired preloads = %v, want %v", plan.Plan.DesiredPreloads, want)
	}
	renderedOrder, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil || !slices.Equal(plan.Plan.DesiredPreloads, renderedOrder) {
		t.Fatalf("copy/managed order differs: %v vs %v, %v", plan.Plan.DesiredPreloads, renderedOrder, err)
	}
}
