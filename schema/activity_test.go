package schema_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/diagnostics"
)

// These tests guard the published field allowlists against Go contract drift.
// They do not replace the runtime value/coverage validation in either package.
type contractSchema struct {
	Type                 string                     `json:"type"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	Properties           map[string]*contractSchema `json:"properties"`
	Required             []string                   `json:"required"`
	Definitions          map[string]*contractSchema `json:"$defs"`
	Pattern              string                     `json:"pattern"`
	MaxLength            int                        `json:"maxLength"`
	MaxItems             int                        `json:"maxItems"`
	Ref                  string                     `json:"$ref"`
	OneOf                []*contractSchema          `json:"oneOf"`
	Const                any                        `json:"const"`
	Items                *contractSchema            `json:"items"`
}

func readContractSchema(t *testing.T, path string) contractSchema {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: callers supply fixed schema filenames in this package.
	if err != nil {
		t.Fatal(err)
	}
	var schema contractSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func assertContractFields(t *testing.T, schema *contractSchema, value any) {
	t.Helper()
	if schema == nil || schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("%T schema must explicitly forbid additional properties", value)
	}
	typ := reflect.TypeOf(value)
	expected := make(map[string]bool, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			t.Fatalf("%T.%s has no explicit JSON field", value, field.Name)
		}
		expected[name] = true
		if _, exists := schema.Properties[name]; !exists {
			t.Errorf("%T.%s missing from schema allowlist", value, field.Name)
		}
		required := !strings.Contains(tag, ",omitempty")
		if slices.Contains(schema.Required, name) != required {
			t.Errorf("%T.%s required-field contract differs from Go JSON tag", value, field.Name)
		}
	}
	for name := range schema.Properties {
		if !expected[name] {
			t.Errorf("schema allows unknown %T field %q", value, name)
		}
	}
	for _, name := range schema.Required {
		if !expected[name] {
			t.Errorf("schema requires unknown %T field %q", value, name)
		}
	}
}

func TestActivitySchemaFieldAllowlists(t *testing.T) {
	schema := readContractSchema(t, "vaire-activity-v1.json")
	assertContractFields(t, schema.Definitions["event"], activity.Event{})
	assertContractFields(t, schema.Definitions["page"], activity.Page{})
	assertContractFields(t, schema.Definitions["coverage"], activity.Coverage{})
	assertContractFields(t, schema.Definitions["health"], activity.Health{})
	assertContractFields(t, schema.Definitions["query"], activity.Query{})
	if got := schema.Definitions["page"].Properties["events"].MaxItems; got != activity.MaxLimit {
		t.Fatalf("schema page cap %d differs from runtime %d", got, activity.MaxLimit)
	}
}

func TestIncidentSchemaFieldAllowlists(t *testing.T) {
	schema := readContractSchema(t, "vaire-incident-v1.json")
	assertContractFields(t, &schema, diagnostics.Bundle{})
	assertContractFields(t, schema.Properties["metadata"], diagnostics.Metadata{})
}

func TestActivitySchemaTokenBounds(t *testing.T) {
	schema := readContractSchema(t, "vaire-activity-v1.json")
	for name, limit := range map[string]int{"label": 128, "shortLabel": 64, "database": 64} {
		def := schema.Definitions[name]
		if def == nil || def.MaxLength != limit || def.Pattern == "" {
			t.Fatalf("%s is not a bounded token", name)
		}
		pattern, err := regexp.Compile(def.Pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"", "SELECT secret FROM users", "https://example.com", "key=value", "one\ntwo", "\x1binstructions"} {
			if pattern.MatchString(bad) {
				t.Errorf("%s schema token permits free-form content", name)
			}
		}
	}
	cursor := schema.Definitions["page"].Properties["next_cursor"]
	if cursor.MaxLength != 2048 || cursor.Pattern == "" {
		t.Fatal("schema cursor is not bounded")
	}
}

func TestActivitySchemaUnderscoreDatabase(t *testing.T) {
	schema := readContractSchema(t, "vaire-activity-v1.json")
	pattern := regexp.MustCompile(schema.Definitions["database"].Pattern)
	for _, name := range []string{"app", "_app", "_app__restoring"} {
		if !pattern.MatchString(name) {
			t.Errorf("database schema rejects %q", name)
		}
	}
	for _, name := range []string{"event", "query"} {
		if schema.Definitions[name].Properties["database"].Ref != "#/$defs/database" {
			t.Errorf("%s database uses the wrong identifier contract", name)
		}
	}
}

func TestFailureEnvelopeSchemaCarriesWarnings(t *testing.T) {
	schema := readContractSchema(t, "vaire-v1.json")
	for _, branch := range schema.OneOf {
		if branch == nil || branch.Properties["ok"] == nil || branch.Properties["ok"].Const != false {
			continue
		}
		warnings := branch.Properties["warnings"]
		if warnings == nil || warnings.Type != "array" || warnings.Items == nil {
			t.Fatalf("failure branch has no bounded warnings array")
		}
		if warnings.Items.Properties["code"] == nil || warnings.Items.Properties["message"] == nil {
			t.Fatalf("failure warnings lack code/message fields: %+v", warnings.Items)
		}
		return
	}
	t.Fatal("vaire-v1.json has no failure branch")
}
