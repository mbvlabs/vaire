package catalog

import (
	"slices"
	"strings"
	"testing"
)

func TestBuiltinsAreConsistent(t *testing.T) {
	names := map[string]bool{}
	for _, e := range List() {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if names[n] {
				t.Fatalf("%s is listed twice", n)
			}
			names[n] = true
		}
		if e.Restart != (e.Preload != "") {
			t.Errorf("%s: Restart must be Preload != \"\"", e.Name)
		}
		if !e.BuiltIn {
			t.Errorf("%s: a built-in entry is not marked BuiltIn", e.Name)
		}
		switch e.Source {
		case SourcePreinstalled:
			if e.Package != "" {
				t.Errorf("%s: a preinstalled entry has package %q", e.Name, e.Package)
			}
		case SourceAPT:
			if !strings.HasPrefix(e.Package, "postgresql-18-") {
				t.Errorf("%s: APT package %q is not a PostgreSQL 18 package", e.Name, e.Package)
			}
		default:
			t.Errorf("%s: unknown source %q", e.Name, e.Source)
		}
		switch e.CopySafety {
		case CopySafetySafe, CopySafetyUnknown:
			if len(e.CopySettings) != 0 {
				t.Errorf("%s: %s has copy settings", e.Name, e.CopySafety)
			}
		case CopySafetyDisabled:
			if len(e.CopySettings) == 0 {
				t.Errorf("%s: disabled copy safety has no copy settings", e.Name)
			}
		default:
			t.Errorf("%s: unknown copy safety %q", e.Name, e.CopySafety)
		}
	}
	for _, want := range []string{"pg_stat_statements", "auto_explain", "vector", "postgis", "pg_cron", "pgaudit", "pg_repack", "pg_partman", "hypopg", "timescaledb"} {
		if !names[want] {
			t.Errorf("%s is missing from the catalogue", want)
		}
	}
}

func TestLookupAliasAndCanonical(t *testing.T) {
	e, ok := Lookup("pgvector")
	if !ok || e.Name != "vector" {
		t.Fatalf("pgvector resolves to %q, %v", e.Name, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("an unknown name resolved")
	}
	if n, ok := Canonical("pgvector"); !ok || n != "vector" {
		t.Fatalf("pgvector: %q %v", n, ok)
	}
	if n, ok := Canonical("nope"); ok || n != "nope" {
		t.Fatalf("nope: %q %v", n, ok)
	}
	got := CanonicalList([]string{"pgvector", " vector", "pg_cron", "nope", ""})
	if !slices.Equal(got, []string{"vector", "pg_cron", "nope"}) {
		t.Fatalf("list: %v", got)
	}
}

func TestDefaultsAndPreloads(t *testing.T) {
	if got := Defaults(); !slices.Equal(got, []string{"pg_stat_statements", "auto_explain"}) {
		t.Fatalf("defaults: %v", got)
	}
	e, _ := Lookup("pg_cron")
	if e.CopySafety != CopySafetyDisabled || e.CopySettings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("pg_cron copy safety: %+v", e)
	}
	if !e.StandbySafe {
		t.Fatal("pg_cron should be standby safe: jobs do not launch on a standby")
	}
}

func TestResolvedSettingsDoesNotMutate(t *testing.T) {
	e, _ := Lookup("pg_cron")
	settings := e.ResolvedSettings("app")
	if settings["cron.database_name"] != "'app'" {
		t.Fatalf("cron.database_name = %q", settings["cron.database_name"])
	}
	if e.Settings["cron.database_name"] != "'"+DBPlaceholder+"'" {
		t.Fatal("ResolvedSettings changed the descriptor")
	}
}

func TestListReturnsDeepCopies(t *testing.T) {
	first := List()
	for i := range first {
		if first[i].Name == "pg_cron" {
			first[i].Settings["cron.database_name"] = "changed"
			first[i].CopySettings["cron.launch_active_jobs"] = "on"
		}
	}
	second := List()
	for _, e := range second {
		if e.Name == "pg_cron" {
			if e.Settings["cron.database_name"] == "changed" {
				t.Fatal("settings alias the catalogue")
			}
			if e.CopySettings["cron.launch_active_jobs"] != "off" {
				t.Fatal("copy settings alias the catalogue")
			}
		}
	}
	e, _ := Lookup("vector")
	e.Aliases[0] = "changed"
	again, _ := Lookup("vector")
	if again.Aliases[0] != "pgvector" {
		t.Fatal("aliases alias the catalogue")
	}
}

func TestSemanticEqual(t *testing.T) {
	a, _ := Lookup("pg_cron")
	b := a.Clone()
	b.Note = "different note"
	if !a.SemanticEqual(b) {
		t.Fatal("a note changed semantic identity")
	}
	b.Settings["cron.max_running_jobs"] = "9"
	if a.SemanticEqual(b) {
		t.Fatal("a setting change kept semantic identity")
	}
	c := a.Clone()
	c.CopySettings = nil
	if a.SemanticEqual(c) {
		t.Fatal("copy settings are not part of semantic identity")
	}
	d := a.Clone()
	d.Settings = map[string]string{}
	e := a.Clone()
	e.Settings = nil
	if !d.SemanticEqual(e) {
		t.Fatal("nil and empty settings are not equivalent")
	}
}

func TestByExtNameAndManaged(t *testing.T) {
	if e, ok := ByExtName("pg_partman"); !ok || e.Schema != "partman" {
		t.Fatalf("pg_partman: %+v %v", e, ok)
	}
	if _, ok := ByExtName(""); ok {
		t.Fatal("auto_explain has no CREATE EXTENSION name")
	}
	if a, _ := Lookup("auto_explain"); a.Managed() {
		t.Fatal("auto_explain is not managed by vaire ext")
	}
}
