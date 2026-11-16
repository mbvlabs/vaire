package steps

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func schemaTestReason(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not typed: %T %v", err, err)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

func TestValidateInstalledExtensionSchemas(t *testing.T) {
	partman := catalog.Extension{Name: "pg_partman", ExtName: "pg_partman", Schema: "partman"}
	if err := ValidateInstalledExtensionSchemas("app", []catalog.Extension{partman}, map[string]string{"pg_partman": "partman"}); err != nil {
		t.Fatalf("matching schema refused: %v", err)
	}
	if err := ValidateInstalledExtensionSchemas("app", []catalog.Extension{partman}, map[string]string{}); err != nil {
		t.Fatalf("fresh extension refused: %v", err)
	}
	err := ValidateInstalledExtensionSchemas("app", []catalog.Extension{partman}, map[string]string{"pg_partman": "public"})
	if err == nil || schemaTestReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("mismatch = %v", err)
	}
	details := err.(*output.CLIError).Details.(map[string]any)
	if details["actual_schema"] != "public" || details["requested_schema"] != "partman" {
		t.Fatalf("mismatch details = %+v", details)
	}
	err = ValidateInstalledExtensionSchemas("app", []catalog.Extension{partman}, map[string]string{"pg_partman": ""})
	if err == nil || schemaTestReason(t, err) != "extension_schema_unknown" {
		t.Fatalf("unknown installed namespace = %v", err)
	}
}

func TestValidateInstalledExtensionSchemasOnlyAssertsDefinedSchema(t *testing.T) {
	for _, tc := range []struct {
		ext        catalog.Extension
		extNSchema string
	}{
		{catalog.Extension{Name: "pg_cron", ExtName: "pg_cron"}, "pg_catalog"},
		{catalog.Extension{Name: "pg_repack", ExtName: "pg_repack"}, "public"},
	} {
		// The actual metadata namespace may differ from the built-in grant or
		// owned-object schema; an unspecified descriptor schema asserts none.
		if err := ValidateInstalledExtensionSchemas("app", []catalog.Extension{tc.ext}, map[string]string{tc.ext.ExtName: tc.extNSchema}); err != nil {
			t.Fatalf("%s optional schema was forced: %v", tc.ext.Name, err)
		}
	}
}

func explicitSchemaConfig(schema string) config.Cluster {
	cfg := rolesPipelineConfig()
	cfg.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Schema: schema,
	}
	return cfg
}

func TestPlanFollowerRuntimeRefusesInstalledSchemaMismatch(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	safe, standby := catalog.CopySafetySafe, true
	cfg.Clusters[0].Extensions = []string{"custom"}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	restoreSQL := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true, "custom_ext": true}, nil
	})
	t.Cleanup(restoreSQL)

	schemas := map[string]string{"custom_ext": "public"}
	restoreSchemas := SetFollowerInstalledExtensionSchemasForTest(func(context.Context, config.ClusterSpec) (map[string]string, error) {
		return schemas, nil
	})
	t.Cleanup(restoreSchemas)

	_, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err == nil || schemaTestReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("follower schema mismatch = %v", err)
	}

	schemas["custom_ext"] = ""
	_, err = PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err == nil || schemaTestReason(t, err) != "extension_schema_unknown" {
		t.Fatalf("follower schema unknown = %v", err)
	}

	schemas["custom_ext"] = "custom"
	if _, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive}); err != nil {
		t.Fatalf("matching follower schema refused: %v", err)
	}
}

func TestPostgresRolesApplyRefusesInstalledSchemaMismatchBeforeSQL(t *testing.T) {
	cfg := explicitSchemaConfig("custom")
	fake := completeRolesPG()
	fake.schemas["custom_ext"] = "public"
	useFakeRolesPG(t, fake)

	_, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || schemaTestReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("installed schema mismatch = %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("roles SQL ran before the schema refusal: %+v", fake.execs)
	}

	fake.schemas["custom_ext"] = "custom"
	if _, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil); err != nil {
		t.Fatalf("matching installed schema refused: %v", err)
	}

	fake.execs = nil
	delete(fake.schemas, "custom_ext")
	_, err = (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || schemaTestReason(t, err) != "extension_schema_unknown" {
		t.Fatalf("unknown installed schema = %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("roles SQL ran with an unknown installed schema: %+v", fake.execs)
	}
}

func TestPostgresRolesApplyAllowsFreshExplicitSchema(t *testing.T) {
	cfg := explicitSchemaConfig("custom")
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	useFakeRolesPG(t, fake)

	if _, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil); err != nil {
		t.Fatalf("fresh explicit schema refused: %v", err)
	}
	if !containsString(fake.extensions, "custom_ext") {
		t.Fatalf("fresh explicit schema did not create SQL: %v", fake.extensions)
	}
}
