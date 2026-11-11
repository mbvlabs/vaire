package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func extensionTestDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	control, lib := filepath.Join(root, "extension"), filepath.Join(root, "lib")
	for _, dir := range []string{control, lib} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	oldControl, oldLib := extensionControlDir, extensionLibDir
	extensionControlDir, extensionLibDir = control, lib
	t.Cleanup(func() {
		extensionControlDir, extensionLibDir = oldControl, oldLib
	})
}

func touchExtensionFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func touchExtensionControl(t *testing.T, name string) {
	t.Helper()
	touchExtensionFile(t, filepath.Join(extensionControlDir, name+".control"))
}

func touchExtensionLibrary(t *testing.T, name string) {
	t.Helper()
	touchExtensionFile(t, filepath.Join(extensionLibDir, name+".so"))
}

func TestParsePreloads(t *testing.T) {
	got := ParsePreloads("'pg_stat_statements, auto_explain'")
	if !got["pg_stat_statements"] || !got["auto_explain"] || len(got) != 2 {
		t.Fatalf("parse = %v", got)
	}
	if empty := ParsePreloads(""); len(empty) != 0 {
		t.Fatalf("empty parse = %v", empty)
	}
}

func TestExtensionPackagesUnion(t *testing.T) {
	cfg := config.DefaultCluster()
	recovery := []string{"recovery_apt"}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Extensions: []string{"custom_apt"}, RecoveryExtensions: &recovery},
		{Name: "other", Extensions: []string{"preinstalled_custom"}},
	}
	cfg.Awaiting = []config.ClusterSpec{{Name: "awaiting", Extensions: []string{"awaiting_apt"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_stat_statements": {
			Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
		"custom_apt":          {Source: catalog.SourceAPT, Package: "postgresql-18-custom"},
		"recovery_apt":        {Source: catalog.SourceAPT, Package: "postgresql-18-recovery"},
		"awaiting_apt":        {Source: catalog.SourceAPT, Package: "postgresql-18-awaiting"},
		"preinstalled_custom": {Source: catalog.SourcePreinstalled},
	}
	packages, err := ExtensionPackages(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"postgresql-18-awaiting", "postgresql-18-custom",
		"postgresql-18-pg-stat-statements", "postgresql-18-recovery",
	}
	if !slices.Equal(packages, want) {
		t.Fatalf("packages = %v, want %v", packages, want)
	}

	baseline, err := ExtensionPackages(config.DefaultCluster())
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 0 {
		t.Fatalf("baseline packages = %v", baseline)
	}
}

func TestExtensionPrerequisitesVerifyFiles(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"pg_cron", "vector", "custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib"},
	}
	reqs, err := ExtensionPrerequisites(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_stat_statements", "pg_cron", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	if err := os.WriteFile(filepath.Join(extensionControlDir, "vector.control"), []byte("module_pathname = '$libdir/vector' # test comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "pg_cron", "vector", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	if err := VerifyExtensionRequirements(reqs); err != nil {
		t.Fatalf("complete files refused: %v", err)
	}

	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	err = VerifyExtensionRequirements(reqs)
	if err == nil || !strings.Contains(err.Error(), `preinstalled extension "custom"`) ||
		!strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("missing preinstalled control = %v", err)
	}

	touchExtensionControl(t, "custom_ext")
	if err := os.Remove(filepath.Join(extensionLibDir, "pg_cron.so")); err != nil {
		t.Fatal(err)
	}
	err = VerifyExtensionRequirements(reqs)
	if err == nil || !strings.Contains(err.Error(), `extension "pg_cron" from package "postgresql-18-cron"`) ||
		!strings.Contains(err.Error(), "pg_cron.so") {
		t.Fatalf("missing package library = %v", err)
	}

	touchExtensionLibrary(t, "pg_cron")
	if err := os.Remove(filepath.Join(extensionLibDir, "vector.so")); err != nil {
		t.Fatal(err)
	}
	err = VerifyExtensionRequirements(reqs)
	if err == nil || !strings.Contains(err.Error(), `extension "vector"`) || !strings.Contains(err.Error(), "vector.so") {
		t.Fatalf("missing control module library = %v", err)
	}
}

func TestExtensionLibraryNamingForms(t *testing.T) {
	extensionTestDirs(t)
	absDir := t.TempDir()
	absLib := filepath.Join(absDir, "abs_lib")
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"suffix_ext", "bare_ext", "abs_ext", "bare_mod", "sql_only"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"suffix_ext": {Source: catalog.SourcePreinstalled, ExtName: "suffix_ext", Preload: "custom_lib.so"},
		"bare_ext":   {Source: catalog.SourcePreinstalled, ExtName: "bare_ext", Preload: "bare_lib"},
		"abs_ext":    {Source: catalog.SourcePreinstalled, ExtName: "abs_ext"},
		"bare_mod":   {Source: catalog.SourcePreinstalled, ExtName: "bare_mod"},
		"sql_only":   {Source: catalog.SourcePreinstalled, ExtName: "sql_only"},
	}
	reqs, err := ExtensionPrerequisites(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_stat_statements", "suffix_ext", "bare_ext", "abs_ext", "bare_mod", "sql_only"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "bare_lib", "bare_mod_lib"} {
		touchExtensionLibrary(t, name)
	}
	touchExtensionFile(t, filepath.Join(extensionLibDir, "custom_lib.so"))
	if err := os.WriteFile(filepath.Join(extensionControlDir, "abs_ext.control"), []byte("module_pathname = '"+absLib+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touchExtensionFile(t, absLib+".so")
	if err := os.WriteFile(filepath.Join(extensionControlDir, "bare_mod.control"), []byte("module_pathname = 'bare_mod_lib'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(reqs); err != nil {
		t.Fatalf("valid naming forms refused: %v", err)
	}

	if err := os.Remove(filepath.Join(extensionLibDir, "custom_lib.so")); err != nil {
		t.Fatal(err)
	}
	err = VerifyExtensionRequirements(reqs)
	if err == nil || !strings.Contains(err.Error(), "custom_lib.so") || strings.Contains(err.Error(), "custom_lib.so.so") {
		t.Fatalf("suffix-qualified preload = %v", err)
	}
	touchExtensionFile(t, filepath.Join(extensionLibDir, "custom_lib.so"))

	if err := os.Remove(filepath.Join(extensionLibDir, "bare_lib.so")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(reqs); err == nil || !strings.Contains(err.Error(), "bare_lib.so") {
		t.Fatalf("bare preload = %v", err)
	}
	touchExtensionLibrary(t, "bare_lib")

	if err := os.Remove(absLib + ".so"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(reqs); err == nil || !strings.Contains(err.Error(), "abs_lib.so") {
		t.Fatalf("absolute module = %v", err)
	}
	touchExtensionFile(t, absLib+".so")

	if err := os.Remove(filepath.Join(extensionLibDir, "bare_mod_lib.so")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(reqs); err == nil || !strings.Contains(err.Error(), "bare_mod_lib.so") {
		t.Fatalf("bare control module = %v", err)
	}
	touchExtensionLibrary(t, "bare_mod_lib")

	unsupported := []ExtensionPrerequisite{{Name: "bad", Source: catalog.SourcePreinstalled, ExtName: "bad_ext"}}
	if err := os.WriteFile(filepath.Join(extensionControlDir, "bad_ext.control"), []byte("module_pathname = '$libdir'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(unsupported); err == nil || !strings.Contains(err.Error(), "unsupported module_pathname") {
		t.Fatalf("unsupported module path = %v", err)
	}

	unreadable := []ExtensionPrerequisite{{Name: "unreadable", Source: catalog.SourcePreinstalled, ExtName: "unreadable"}}
	if err := os.MkdirAll(filepath.Join(extensionControlDir, "unreadable.control"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := VerifyExtensionRequirements(unreadable); err == nil || !strings.Contains(err.Error(), "cannot read control file") {
		t.Fatalf("unreadable control file = %v", err)
	}
}

func TestPostgresPackagesInputsPreserveBaseline(t *testing.T) {
	baseline, err := (postgresPackages{}).Inputs(config.DefaultCluster(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(baseline, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["extension_packages"]; ok {
		t.Fatal("baseline package inputs gained an extension_packages key")
	}

	preinstalled := config.DefaultCluster()
	preinstalled.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	preinstalled.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled},
	}
	preinstalledInputs, err := (postgresPackages{}).Inputs(preinstalled, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(preinstalledInputs) != string(baseline) {
		t.Fatal("a preinstalled-only selection changed the package step's baseline inputs")
	}

	apt := config.DefaultCluster()
	apt.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	apt.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom"},
	}
	aptInputs, err := (postgresPackages{}).Inputs(apt, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(aptInputs, &raw); err != nil {
		t.Fatal(err)
	}
	got, ok := raw["extension_packages"].([]any)
	if !ok || len(got) != 1 || got[0] != "postgresql-18-custom" {
		t.Fatalf("extension_packages = %#v", raw["extension_packages"])
	}
}

func TestPostgresPackagesCheckUsesRealFilesAndHolds(t *testing.T) {
	extensionTestDirs(t)
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}

	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom_ext"},
	}
	oldProbe := packageProbe
	var probes []string
	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		probes = append(probes, name+" "+strings.Join(args, " "))
		switch name {
		case "dpkg-query":
			return "install ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n") + "\npostgresql-18-custom\n", nil
		}
		return "", nil
	}
	t.Cleanup(func() { packageProbe = oldProbe })

	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("complete package state = %+v, %v", res, err)
	}
	for _, want := range []string{
		"dpkg-query -W -f=${Status} postgresql-18",
		"dpkg-query -W -f=${Status} postgresql-18-custom",
		"apt-mark showhold",
	} {
		if !slices.Contains(probes, want) {
			t.Errorf("missing exact probe %q in %v", want, probes)
		}
	}

	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			return "install ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n"), nil
		}
		return "", nil
	}
	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "is not held") {
		t.Fatalf("missing hold = %+v, %v", res, err)
	}

	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			return "install ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n") + "\npostgresql-18-custom\n", nil
		}
		return "", nil
	}
	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "custom_ext.control") {
		t.Fatalf("missing control file = %+v, %v", res, err)
	}
}

func TestPostgresPackagesScriptEnvAndFailureHonesty(t *testing.T) {
	extensionTestDirs(t)
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom_ext"},
	}

	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	var got map[string]string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		got = env
		return nil
	}
	if err := installPostgresPackages(context.Background(), cfg, "postgres-packages-v1", engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if got["EXTENSION_PACKAGES"] != "postgresql-18-custom" ||
		!strings.Contains(got["PACKAGES"], "postgresql-18") ||
		!strings.Contains(got["HOLD"], "pgbackrest") ||
		got["PGDG_SCRIPT"] != pgdgScript ||
		got["STOP_PGBOUNCER"] != "1" {
		t.Fatalf("package script env = %#v", got)
	}

	// The script can succeed and leave packages installed while the real
	// file verification fails; the error must say that installed packages
	// remain, not claim nothing changed.
	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		return nil
	}
	err := installPostgresPackages(context.Background(), cfg, "postgres-packages-v1", engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "extension prerequisites are not met") ||
		!strings.Contains(err.Error(), "installed packages are not removed") {
		t.Fatalf("post-install verification failure = %v", err)
	}

	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		return &engine.ScriptError{Step: "postgres-packages-v1", Status: 100, Tail: []string{"E: Unable to locate package postgresql-18-custom"}}
	}
	err = installPostgresPackages(context.Background(), cfg, "postgres-packages-v1", engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Unable to locate package") ||
		!strings.Contains(err.Error(), "installed packages are not removed") {
		t.Fatalf("package failure = %v", err)
	}
}

func TestPrepareExtensionStartScopedAPT(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom_ext", Preload: "custom_lib"},
	}
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	var got map[string]string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		got = env
		return nil
	}
	if err := PrepareExtensionStart(context.Background(), cfg, cfg.Clusters[0], nil, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if got["EXTENSION_PACKAGES"] != "postgresql-18-custom" || got["PACKAGES"] != "" ||
		got["HOLD"] != "" || got["PGDG_SCRIPT"] != "" || got["PGBACKREST_MIN"] != "" ||
		got["STOP_PGBOUNCER"] != "0" {
		t.Fatalf("scoped prestart env = %#v", got)
	}
}

func TestPrepareExtensionStartExtraAndPreinstalled(t *testing.T) {
	extensionTestDirs(t)
	for _, name := range []string{"pg_stat_statements"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app"}}

	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	var got map[string]string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		got = env
		return nil
	}
	touchExtensionControl(t, "extra_ext")
	touchExtensionLibrary(t, "extra_lib")
	extra := []ExtensionPrerequisite{{
		Name: "extra", Source: catalog.SourceAPT, Package: "postgresql-18-extra",
		ExtName: "extra_ext", Preload: "extra_lib",
	}}
	if err := PrepareExtensionStart(context.Background(), cfg, cfg.Clusters[0], extra, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if got["EXTENSION_PACKAGES"] != "postgresql-18-extra" || got["PACKAGES"] != "" ||
		got["PGDG_SCRIPT"] != "" || got["STOP_PGBOUNCER"] != "0" {
		t.Fatalf("prestart env = %#v", got)
	}

	// A preinstalled-only requirement performs no install but verifies the
	// actual file and reports it explicitly when absent.
	got = nil
	if err := os.Remove(filepath.Join(extensionControlDir, "extra_ext.control")); err != nil {
		t.Fatal(err)
	}
	preinstalled := []ExtensionPrerequisite{{
		Name: "custom", Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
	}}
	err := PrepareExtensionStart(context.Background(), cfg, cfg.Clusters[0], preinstalled, engine.Runtime{}, nil)
	if got != nil {
		t.Fatalf("preinstalled-only prestart ran the package script: %#v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("missing preinstalled file = %v", err)
	}
}

func TestExtensionSQLSchemaGrantsAndDefaults(t *testing.T) {
	custom := catalog.Extension{Name: "custom", ExtName: "my_ext", Schema: "myext"}
	sql := ExtensionSQL(custom, "app")
	for _, want := range []string{
		`CREATE SCHEMA IF NOT EXISTS "myext";`,
		`CREATE EXTENSION IF NOT EXISTS "my_ext" SCHEMA "myext";`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("custom SQL missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "GRANT") {
		t.Fatalf("custom extension gained a grant:\n%s", sql)
	}

	cron := ExtensionSQL(catalog.Extension{Name: "pg_cron", ExtName: "pg_cron"}, "app")
	if !strings.Contains(cron, `GRANT USAGE ON SCHEMA "cron" TO "app_owner";`) {
		t.Fatalf("pg_cron grant missing:\n%s", cron)
	}
	partman := ExtensionSQL(catalog.Extension{Name: "pg_partman", ExtName: "pg_partman", Schema: "partman"}, "app")
	if !strings.Contains(partman, `CREATE SCHEMA IF NOT EXISTS "partman";`) ||
		!strings.Contains(partman, `GRANT ALL ON SCHEMA "partman" TO "app_owner";`) {
		t.Fatalf("pg_partman SQL:\n%s", partman)
	}

	// An accepted schema override grants the resolved schema, not the
	// built-in default.
	override := config.DefaultCluster()
	override.Extensions = map[string]config.ExtensionDefinition{
		"pg_partman": {Schema: "custom_partman"},
	}
	partmanExt, err := override.ExtensionResolver().Resolve("pg_partman")
	if err != nil {
		t.Fatal(err)
	}
	partmanOverride := ExtensionSQL(partmanExt, "app")
	for _, want := range []string{
		`CREATE SCHEMA IF NOT EXISTS "custom_partman";`,
		`CREATE EXTENSION IF NOT EXISTS "pg_partman" SCHEMA "custom_partman";`,
		`GRANT ALL ON SCHEMA "custom_partman" TO "app_owner";`,
		`GRANT ALL ON ALL TABLES IN SCHEMA "custom_partman" TO "app_owner";`,
	} {
		if !strings.Contains(partmanOverride, want) {
			t.Errorf("pg_partman override missing %q:\n%s", want, partmanOverride)
		}
	}
	if strings.Contains(partmanOverride, `"partman"`) {
		t.Fatalf("pg_partman override granted the default schema:\n%s", partmanOverride)
	}
	// A fresh built-in resolves the default schema.
	fresh, err := config.DefaultCluster().ExtensionResolver().Resolve("pg_partman")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Schema != "partman" || !strings.Contains(ExtensionSQL(fresh, "app"), `GRANT ALL ON SCHEMA "partman" TO "app_owner";`) {
		t.Fatalf("fresh pg_partman schema = %q", fresh.Schema)
	}
	if auto := ExtensionSQL(catalog.Extension{Name: "auto_explain", Preload: "auto_explain"}, "app"); auto != "" {
		t.Fatalf("preload-only module produced SQL: %q", auto)
	}

	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Schema: "myext"},
	}
	db := config.ClusterSpec{Name: "app", Extensions: []string{"custom"}, Protected: true}
	grants, err := databaseGrantsSQL(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";`,
		`CREATE SCHEMA IF NOT EXISTS "myext";`,
		`CREATE EXTENSION IF NOT EXISTS "custom_ext" SCHEMA "myext";`,
	} {
		if !strings.Contains(grants, want) {
			t.Errorf("effective grants missing %q:\n%s", want, grants)
		}
	}
	if strings.Contains(grants, `CREATE EXTENSION IF NOT EXISTS "auto_explain"`) {
		t.Fatalf("auto_explain was created:\n%s", grants)
	}
	if strings.Contains(grants, "vaire_admin") {
		t.Fatalf("extension SQL widened vaire_admin:\n%s", grants)
	}
}

func TestExtensionCreationSQLPreloadDeferral(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"preload_ext", "plain"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"preload_ext": {Source: catalog.SourcePreinstalled, Preload: "preload_lib"},
		"plain":       {Source: catalog.SourcePreinstalled},
	}
	spec := cfg.Clusters[0]
	baseLoaded := map[string]bool{"pg_stat_statements": true, "auto_explain": true}

	sql, deferred, err := extensionCreationSQL(cfg, spec, map[string]bool{
		"pg_stat_statements": true, "auto_explain": true, "preload_lib": true,
	}, nil)
	if err != nil || len(deferred) != 0 || !strings.Contains(sql, `"preload_ext"`) || !strings.Contains(sql, `"plain"`) {
		t.Fatalf("loaded preload SQL = %q, %v, %v", sql, deferred, err)
	}

	sql, deferred, err = extensionCreationSQL(cfg, spec, baseLoaded, map[string]bool{"shared_preload_libraries": true})
	if err != nil || !slices.Equal(deferred, []string{"preload_ext"}) ||
		strings.Contains(sql, `"preload_ext"`) || !strings.Contains(sql, `"plain"`) {
		t.Fatalf("deferred SQL = %q, %v, %v", sql, deferred, err)
	}

	_, _, err = extensionCreationSQL(cfg, spec, baseLoaded, nil)
	if err == nil || !strings.Contains(err.Error(), "preload_lib") || !strings.Contains(err.Error(), "no restart is pending") {
		t.Fatalf("missing library without pending restart = %v", err)
	}

	// A stale pending restart for another library does not defer an
	// extension whose own library is already effective.
	sql, deferred, err = extensionCreationSQL(cfg, spec, map[string]bool{
		"pg_stat_statements": true, "auto_explain": true, "preload_lib": true,
	}, map[string]bool{"shared_preload_libraries": true})
	if err != nil || len(deferred) != 0 || !strings.Contains(sql, `"preload_ext"`) {
		t.Fatalf("stale pending SQL = %q, %v, %v", sql, deferred, err)
	}
}

func TestMissingExtensionNamesUsesEffectiveSelection(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"custom"}}

	missing, err := missingExtensionNames(cfg, spec, map[string]bool{"pg_stat_statements": true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"custom_ext"}) {
		t.Fatalf("missing = %v", missing)
	}
	missing, err = missingExtensionNames(cfg, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"custom_ext", "pg_stat_statements"}) {
		t.Fatalf("missing with empty installed = %v", missing)
	}
	if slices.Contains(missing, "auto_explain") {
		t.Fatalf("preload-only auto_explain was required as SQL: %v", missing)
	}
}
