package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

type rolesInputsCase struct {
	name          string
	defs          map[string]config.ExtensionDefinition
	clusters      []config.ClusterSpec
	equalBaseline bool
	contains      []string
}

func rolesIdentityBase() config.Cluster {
	return config.Cluster{Clusters: []config.ClusterSpec{{
		Name: "app", Extensions: []string{"pg_stat_statements"},
	}}}
}

func rolesInputsFor(t *testing.T, cfg config.Cluster) []byte {
	t.Helper()
	inputs, err := (postgresRoles{}).Instance("app").Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

func assertRolesInputsCase(t *testing.T, base config.Cluster, baseline []byte, tc rolesInputsCase) {
	t.Helper()
	cfg := base.Clone()
	cfg.Extensions = tc.defs
	if tc.clusters != nil {
		cfg.Clusters = tc.clusters
	}
	got := rolesInputsFor(t, cfg)
	if tc.equalBaseline {
		if string(got) != string(baseline) {
			t.Fatalf("%s changed roles inputs", tc.name)
		}
		return
	}
	for _, want := range tc.contains {
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s missing from roles inputs:\n%s", want, got)
		}
	}
}

func TestRolesInputsCarryOnlySemanticCustomIdentity(t *testing.T) {
	base := rolesIdentityBase()
	baseline := rolesInputsFor(t, base)
	if strings.Contains(string(baseline), "extension_identity") {
		t.Fatalf("baseline inputs gained an identity:\n%s", baseline)
	}
	for _, tc := range []rolesInputsCase{
		{
			name:          "note-only built-in override",
			defs:          map[string]config.ExtensionDefinition{"pg_stat_statements": {Note: "human note"}},
			equalBaseline: true,
		},
		{
			name:          "note-only implicit auto_explain override",
			defs:          map[string]config.ExtensionDefinition{"auto_explain": {Note: "human note"}},
			equalBaseline: true,
		},
		{
			name: "implicit auto_explain provider change",
			defs: map[string]config.ExtensionDefinition{
				"auto_explain": {
					Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
					CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
				},
			},
			contains: []string{`"name":"auto_explain"`, "postgresql-18-auto-explain"},
		},
		{
			name: "implicit auto_explain safety change",
			defs: map[string]config.ExtensionDefinition{
				"auto_explain": {CopySafety: stringPtr(catalog.CopySafetyUnknown)},
			},
			contains: []string{`"name":"auto_explain"`, `"copy_safety":"unknown"`},
		},
		{
			name: "provider change",
			defs: map[string]config.ExtensionDefinition{
				"pg_stat_statements": {
					Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements",
					CopySafety: stringPtr("safe"), StandbySafe: boolPtr(true),
				},
			},
			contains: []string{"extension_identity", "postgresql-18-pg-stat-statements"},
		},
		{
			name: "schema change",
			defs: map[string]config.ExtensionDefinition{
				"pg_stat_statements": {Schema: "custom"},
			},
			contains: []string{`"schema":"custom"`},
		},
		{
			name: "safety change",
			defs: map[string]config.ExtensionDefinition{
				"pg_stat_statements": {CopySafety: stringPtr(catalog.CopySafetyUnknown)},
			},
			contains: []string{`"copy_safety":"unknown"`},
		},
		{
			name:          "unused definition",
			defs:          map[string]config.ExtensionDefinition{"unused": {Source: catalog.SourcePreinstalled}},
			equalBaseline: true,
		},
		{
			name: "custom identity",
			clusters: []config.ClusterSpec{{
				Name: "app", Extensions: []string{"custom"},
			}},
			defs: map[string]config.ExtensionDefinition{
				"custom": {Source: catalog.SourcePreinstalled, Settings: map[string]string{"custom.mode": "on"}},
			},
			contains: []string{"extension_identity", "custom.mode"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRolesInputsCase(t, base, baseline, tc)
		})
	}

	custom := base.Clone()
	custom.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	custom.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, Settings: map[string]string{"custom.mode": "on"}},
	}
	withCustom := rolesInputsFor(t, custom)
	withCustomNote := custom.Clone()
	withCustomNote.Extensions["custom"] = func() config.ExtensionDefinition {
		d := withCustomNote.Extensions["custom"]
		d.Note = "different note"
		return d
	}()
	if noteAgain := rolesInputsFor(t, withCustomNote); string(noteAgain) != string(withCustom) {
		t.Fatal("a custom note changed roles inputs")
	}
}

func TestConfigInputsCarrySemanticIdentity(t *testing.T) {
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{
		Name: "app", Size: "small", Port: 5433, StatementTimeout: "60s",
		Extensions: []string{"pg_stat_statements"},
	}}
	step := postgresConfig{}.Instance("app")
	baseline, err := step.Inputs(base, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseline), "extension_identity") {
		t.Fatalf("baseline config inputs gained an identity:\n%s", baseline)
	}
	note := base.Clone()
	note.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {Note: "human note"},
	}
	withNote, err := step.Inputs(note, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(withNote) != string(baseline) {
		t.Fatal("a note-only override changed config inputs")
	}
	implicitNote := base.Clone()
	implicitNote.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {Note: "human note"},
	}
	withImplicitNote, err := step.Inputs(implicitNote, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(withImplicitNote) != string(baseline) {
		t.Fatal("a note-only implicit auto_explain override changed config inputs")
	}
	implicit := base.Clone()
	implicit.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {
			Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	withImplicit, err := step.Inputs(implicit, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withImplicit), `"name":"auto_explain"`) ||
		!strings.Contains(string(withImplicit), "postgresql-18-auto-explain") {
		t.Fatalf("implicit auto_explain provider change missing from config inputs:\n%s", withImplicit)
	}
	override := base.Clone()
	override.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {Schema: "custom"},
	}
	withSchema, err := step.Inputs(override, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withSchema), "extension_identity") || !strings.Contains(string(withSchema), `"schema":"custom"`) {
		t.Fatalf("schema change missing from config inputs:\n%s", withSchema)
	}
}

func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }

func TestDatabaseGrantsSQLQuotesSafeExtensionNames(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"uuid-ossp": {Source: catalog.SourcePreinstalled},
		"mixed_ext": {Source: catalog.SourcePreinstalled, ExtName: "Mixed.Ext!"},
	}
	sql, err := DatabaseGrantsSQL(cfg, config.ClusterSpec{Name: "app", Extensions: []string{"uuid-ossp", "mixed_ext"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`,
		`CREATE EXTENSION IF NOT EXISTS "Mixed.Ext!";`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in:\n%s", want, sql)
		}
	}
}

func TestDatabaseGrantsSQLResolvesCustomDefinitions(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "my_ext"},
	}
	sql, err := DatabaseGrantsSQL(cfg, config.ClusterSpec{Name: "app", Extensions: []string{"custom"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `CREATE EXTENSION IF NOT EXISTS "my_ext";`) {
		t.Fatalf("custom extension missing:\n%s", sql)
	}
	sql, err = DatabaseGrantsSQL(cfg, config.ClusterSpec{Name: "app", Extensions: []string{"nope"}})
	if err == nil {
		t.Fatal("unknown extension was silently skipped")
	}
	if sql != "" {
		t.Fatalf("invalid intent emitted SQL: %q", sql)
	}
}
