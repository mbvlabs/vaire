package steps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// extensionFixture writes PG18's control and default-version SQL files for
// one extension.
func extensionFixture(t *testing.T, dir, name, version, schema, requires string) {
	t.Helper()
	control := "comment = 'fixture'\ndefault_version = '" + version + "'\n"
	if schema != "" {
		control += "schema = '" + schema + "'\n"
	}
	if requires != "" {
		control += "requires = '" + requires + "'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name+".control"), []byte(control), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"--"+version+".sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withExtensionControlDir(t *testing.T, dir string) {
	t.Helper()
	old := pgExtensionControlDir
	pgExtensionControlDir = dir
	t.Cleanup(func() { pgExtensionControlDir = old })
}

func TestCopyExtensionPlanMatchesInstalledMetadata(t *testing.T) {
	dir := t.TempDir()
	withExtensionControlDir(t, dir)
	extensionFixture(t, dir, "plpgsql", "1.0", "pg_catalog", "")
	extensionFixture(t, dir, "pg_stat_statements", "1.10", "", "")
	extensionFixture(t, dir, "pg_trgm", "1.6", "", "")

	plan, configured, err := CopyExtensionPlan([]ExtensionState{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ExtensionState{
		{Name: "pg_stat_statements", Version: "1.10", Schema: "public"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
	}
	if len(plan) != len(want) {
		t.Fatalf("plan = %+v", plan)
	}
	for i := range want {
		if plan[i] != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, plan[i], want[i])
		}
	}
	if strings.Join(configured, ",") != "pg_stat_statements,pg_trgm" {
		t.Errorf("configured = %v", configured)
	}
}

func TestCopyExtensionPlanFollowsInstallChain(t *testing.T) {
	dir := t.TempDir()
	withExtensionControlDir(t, dir)
	extensionFixture(t, dir, "plpgsql", "1.0", "pg_catalog", "")
	extensionFixture(t, dir, "pg_stat_statements", "1.10", "", "")
	if err := os.WriteFile(filepath.Join(dir, "pg_trgm.control"), []byte("default_version = '1.6'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_trgm--1.4.sql", "pg_trgm--1.4--1.5.sql", "pg_trgm--1.5--1.6.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, _, err := CopyExtensionPlan([]ExtensionState{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ext := range plan {
		if ext.Name == "pg_trgm" && ext.Version == "1.6" {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestCopyExtensionPlanRejectsBrokenInstallChain(t *testing.T) {
	dir := t.TempDir()
	withExtensionControlDir(t, dir)
	extensionFixture(t, dir, "plpgsql", "1.0", "pg_catalog", "")
	extensionFixture(t, dir, "pg_stat_statements", "1.10", "", "")
	if err := os.WriteFile(filepath.Join(dir, "pg_trgm.control"), []byte("default_version = '1.6'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_trgm--1.4.sql", "pg_trgm--1.4--1.5.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := CopyExtensionPlan([]ExtensionState{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
	})
	if err == nil {
		t.Fatal("a broken install chain was accepted")
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a typed error: %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != "extension_version_incompatible" {
		t.Fatalf("reason = %v", details["reason"])
	}
}

func TestCopyExtensionPlanDependencyOrder(t *testing.T) {
	dir := t.TempDir()
	withExtensionControlDir(t, dir)
	extensionFixture(t, dir, "plpgsql", "1.0", "pg_catalog", "")
	extensionFixture(t, dir, "pg_stat_statements", "1.10", "", "")
	extensionFixture(t, dir, "btree_gin", "1.3", "", "pg_trgm")
	extensionFixture(t, dir, "pg_trgm", "1.6", "", "")

	_, configured, err := CopyExtensionPlan([]ExtensionState{
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		{Name: "btree_gin", Version: "1.3", Schema: "public"},
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
	})
	if err != nil {
		t.Fatal(err)
	}
	seenTrgm := false
	for _, name := range configured {
		if name == "pg_trgm" {
			seenTrgm = true
		}
		if name == "btree_gin" && !seenTrgm {
			t.Fatalf("btree_gin was ordered before its dependency: %v", configured)
		}
	}
}

func TestCopyExtensionPlanMismatches(t *testing.T) {
	dir := t.TempDir()
	withExtensionControlDir(t, dir)
	extensionFixture(t, dir, "plpgsql", "1.0", "pg_catalog", "")
	extensionFixture(t, dir, "pg_stat_statements", "1.10", "", "")
	extensionFixture(t, dir, "pg_trgm", "1.6", "", "")

	cases := []struct {
		name   string
		source []ExtensionState
		reason string
	}{
		{"version", []ExtensionState{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}, {Name: "pg_trgm", Version: "1.5", Schema: "public"}}, "extension_version_incompatible"},
		{"schema", []ExtensionState{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}, {Name: "pg_trgm", Version: "1.6", Schema: "extensions"}}, "extension_schema_incompatible"},
		{"unsupported", []ExtensionState{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}, {Name: "postgis", Version: "3.4", Schema: "public"}}, "extension_missing"},
		{"duplicate", []ExtensionState{{Name: "pg_trgm", Version: "1.6", Schema: "public"}, {Name: "pg_trgm", Version: "1.6", Schema: "public"}}, "extension_incompatible"},
		{"missing_control", []ExtensionState{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}, {Name: "citext", Version: "1.6", Schema: "public"}}, "extension_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CopyExtensionPlan(tc.source)
			if err == nil {
				t.Fatal("expected an error")
			}
			var cliErr *output.CLIError
			if !errors.As(err, &cliErr) {
				t.Fatalf("not a typed error: %v", err)
			}
			details, _ := cliErr.Details.(map[string]any)
			if details["reason"] != tc.reason {
				t.Fatalf("reason = %v, want %q (%v)", details["reason"], tc.reason, err)
			}
		})
	}
}

func TestCopyObjectSQLQuotesAndExcludes(t *testing.T) {
	grants := CopyObjectGrantsSQL("app")
	for _, want := range []string{
		`GRANT USAGE ON SCHEMA %I TO "app_app", "app_ro"`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app_owner" IN SCHEMA %I`,
		`c.relowner = 'app_owner'::regrole`,
		`pg_get_function_identity_arguments`,
		`d.deptype = 'e'`,
	} {
		if !strings.Contains(grants, want) {
			t.Errorf("grants SQL is missing %q", want)
		}
	}
	for _, forbidden := range []string{"GRANT ALL", "SUPERUSER", "postgres_app", "source_owner"} {
		if strings.Contains(grants, forbidden) {
			t.Errorf("grants SQL contains %q", forbidden)
		}
	}

	verify := CopyObjectVerifySQL("app")
	if strings.Contains(verify, "has_foreign_table_privilege") {
		t.Fatal("verification SQL uses the nonexistent has_foreign_table_privilege function")
	}
	for _, want := range []string{
		`has_schema_privilege('app_app', s.oid, 'USAGE')`,
		`has_table_privilege('app_app', c.oid, 'SELECT,INSERT,UPDATE,DELETE')`,
		`has_table_privilege('app_ro', c.oid, 'SELECT')`,
		`has_sequence_privilege('app_app', c.oid, 'USAGE')`,
		`has_function_privilege('app_app', p.oid, 'EXECUTE')`,
		`pg_get_userbyid(datdba) <> 'app_owner'`,
		`has_database_privilege('app_app', datname, 'CONNECT')`,
		`pg_has_role('vaire_admin', 'app_owner'::regrole, 'MEMBER')`,
	} {
		if !strings.Contains(verify, want) {
			t.Errorf("verify SQL is missing %q", want)
		}
	}

	analyze := CopyAnalyzeSQL("app")
	if !strings.Contains(analyze, `SET ROLE "app_owner"`) || !strings.Contains(analyze, "ANALYZE") {
		t.Errorf("analyze SQL = %q", analyze)
	}
}
