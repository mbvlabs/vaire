package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestRolesInputsCarryOnlySemanticCustomIdentity(t *testing.T) {
	base := config.Cluster{Clusters: []config.ClusterSpec{{
		Name: "app", Extensions: []string{"pg_stat_statements"},
	}}}
	step := postgresRoles{}.Instance("app")
	baseline, err := step.Inputs(base, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(baseline), "extension_identity") {
		t.Fatalf("baseline inputs gained an identity:\n%s", baseline)
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
		t.Fatal("a note-only built-in override changed roles inputs")
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
		t.Fatal("a note-only implicit auto_explain override changed roles inputs")
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
		t.Fatalf("implicit auto_explain provider change missing from roles inputs:\n%s", withImplicit)
	}

	implicitSafety := base.Clone()
	implicitSafety.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {CopySafety: stringPtr(catalog.CopySafetyUnknown)},
	}
	withImplicitSafety, err := step.Inputs(implicitSafety, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withImplicitSafety), `"name":"auto_explain"`) ||
		!strings.Contains(string(withImplicitSafety), `"copy_safety":"unknown"`) {
		t.Fatalf("implicit auto_explain safety change missing from roles inputs:\n%s", withImplicitSafety)
	}

	override := base.Clone()
	override.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {
			Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements",
			CopySafety: stringPtr("safe"), StandbySafe: boolPtr(true),
		},
	}
	withProvider, err := step.Inputs(override, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withProvider), "extension_identity") ||
		!strings.Contains(string(withProvider), "postgresql-18-pg-stat-statements") {
		t.Fatalf("provider change missing from roles inputs:\n%s", withProvider)
	}

	schema := base.Clone()
	schema.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {Schema: "custom"},
	}
	withSchema, err := step.Inputs(schema, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withSchema), `"schema":"custom"`) {
		t.Fatalf("schema change missing from roles inputs:\n%s", withSchema)
	}

	safety := base.Clone()
	safety.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {CopySafety: stringPtr(catalog.CopySafetyUnknown)},
	}
	withSafety, err := step.Inputs(safety, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withSafety), `"copy_safety":"unknown"`) {
		t.Fatalf("safety change missing from roles inputs:\n%s", withSafety)
	}

	custom := base.Clone()
	custom.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	custom.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, Settings: map[string]string{"custom.mode": "on"}},
	}
	withCustom, err := step.Inputs(custom, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withCustom), "extension_identity") || !strings.Contains(string(withCustom), "custom.mode") {
		t.Fatalf("custom identity missing from roles inputs:\n%s", withCustom)
	}

	withCustomNote := custom.Clone()
	withCustomNote.Extensions["custom"] = func() config.ExtensionDefinition {
		d := withCustomNote.Extensions["custom"]
		d.Note = "different note"
		return d
	}()
	noteAgain, err := step.Inputs(withCustomNote, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(noteAgain) != string(withCustom) {
		t.Fatal("a custom note changed roles inputs")
	}

	unused := base.Clone()
	unused.Extensions = map[string]config.ExtensionDefinition{
		"unused": {Source: catalog.SourcePreinstalled},
	}
	withUnused, err := step.Inputs(unused, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(withUnused) != string(baseline) {
		t.Fatal("an unused definition changed roles inputs")
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
