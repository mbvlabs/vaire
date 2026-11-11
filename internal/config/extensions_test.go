package config

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
)

func extensionCluster(t *testing.T) Cluster {
	t.Helper()
	cfg := DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []NodeSpec{{Name: "db1", Role: RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []ClusterSpec{{
		Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s",
		Extensions: []string{"pg_stat_statements"},
	}}
	return cfg
}

func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }

func addCustom(t *testing.T, cfg *Cluster, name string, def ExtensionDefinition) {
	t.Helper()
	if cfg.Extensions == nil {
		cfg.Extensions = map[string]ExtensionDefinition{}
	}
	cfg.Extensions[name] = def
}

func hostClusterSpec(name string, port int, extensions ...string) ClusterSpec {
	return ClusterSpec{
		Name: name, Node: "db1", Port: port, Stanza: name + "-abc123",
		Size: "small", StatementTimeout: "60s", Extensions: extensions,
	}
}

func hostExtensionCluster(t *testing.T, defs map[string]ExtensionDefinition, dbs ...ClusterSpec) Cluster {
	t.Helper()
	cfg := extensionCluster(t)
	cfg.Clusters = dbs
	for name, def := range defs {
		addCustom(t, &cfg, name, def)
	}
	return cfg
}

func sharedSQLDefinition(preload string, settings map[string]string) ExtensionDefinition {
	return ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-shared",
		ExtName: "shared", Preload: preload, Settings: settings,
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	}
}

func TestExtensionDefinitionsAndRecoveryRoundTrip(t *testing.T) {
	cfg := extensionCluster(t)
	addCustom(t, &cfg, "pg_stat_kcache", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-kcache",
		Preload: "pg_stat_kcache", Settings: map[string]string{"pg_stat_kcache.track": "all"},
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		Note: "example custom APT extension",
	})
	addCustom(t, &cfg, "myext", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "my_ext", Schema: "myext",
		Settings: map[string]string{"myext.mode": "'safe'"}, Note: "operator-installed",
	})
	cfg.Clusters[0].Extensions = []string{"pg_stat_statements", "pg_stat_kcache", "myext"}
	empty := []string{}
	cfg.Clusters[0].RecoveryExtensions = &empty
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid custom definitions refused: %v", err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[extensions.pg_stat_kcache]") || !strings.Contains(string(raw), "recovery_extensions = []") {
		t.Fatalf("encoded TOML lacks the new sections:\n%s", raw)
	}
	got, err := ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Extensions["pg_stat_kcache"].Package != "postgresql-18-pg-stat-kcache" {
		t.Fatalf("custom APT definition did not round-trip: %+v", got.Extensions["pg_stat_kcache"])
	}
	if got.Extensions["myext"].Source != catalog.SourcePreinstalled || got.Extensions["myext"].ExtName != "my_ext" {
		t.Fatalf("custom preinstalled definition did not round-trip: %+v", got.Extensions["myext"])
	}
	if got.Clusters[0].RecoveryExtensions == nil || len(*got.Clusters[0].RecoveryExtensions) != 0 {
		t.Fatalf("explicitly empty recovery_extensions became %v", got.Clusters[0].RecoveryExtensions)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("round-tripped config is invalid: %v", err)
	}
}

func TestRecoveryExtensionsPresence(t *testing.T) {
	base := extensionCluster(t)
	cases := []struct {
		name  string
		value *[]string
		check func(*testing.T, *[]string)
	}{
		{name: "absent", value: nil, check: func(t *testing.T, got *[]string) {
			if got != nil {
				t.Fatalf("absent became %v", got)
			}
		}},
		{name: "explicit empty", value: &[]string{}, check: func(t *testing.T, got *[]string) {
			if got == nil || len(*got) != 0 {
				t.Fatalf("explicit empty became %v", got)
			}
		}},
		{name: "populated", value: &[]string{"pg_trgm", "vector"}, check: func(t *testing.T, got *[]string) {
			if got == nil || !slices.Equal(*got, []string{"pg_trgm", "vector"}) {
				t.Fatalf("populated became %v", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Clusters = slices.Clone(base.Clusters)
			cfg.Clusters[0].RecoveryExtensions = tc.value
			raw, err := cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseCluster(raw)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, got.Clusters[0].RecoveryExtensions)
			again, err := got.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw, again) {
				t.Fatalf("presence did not survive a second round trip:\n%s\n---\n%s", raw, again)
			}
		})
	}
}

// Every assignment in the security template is a managed setting: an extension
// descriptor must not be able to override the policy that template renders.
func TestSecurityTemplateSettingsAreReserved(t *testing.T) {
	raw, err := os.ReadFile("../render/templates/20-vaire-security.conf.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("security template line is not an assignment: %q", line)
		}
		key = strings.TrimSpace(key)
		seen++
		if !isReservedSetting(key) {
			t.Errorf("security template setting %q is not reserved", key)
		}
	}
	if seen == 0 {
		t.Fatal("the security template has no assignments")
	}
}

func TestExtensionDefinitionValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Cluster)
		want string
	}{
		{
			name: "built-in alias in extensions",
			edit: func(c *Cluster) { c.Clusters[0].Extensions = []string{"pgvector"} },
			want: `"pgvector" must be written as "vector"`,
		},
		{
			name: "unknown extension",
			edit: func(c *Cluster) { c.Clusters[0].Extensions = []string{"nope"} },
			want: `"nope" is not built in`,
		},
		{
			name: "duplicate extension",
			edit: func(c *Cluster) { c.Clusters[0].Extensions = []string{"pg_trgm", "pg_trgm"} },
			want: `"pg_trgm" is listed twice`,
		},
		{
			name: "definition key is a built-in alias",
			edit: func(c *Cluster) {
				addCustom(t, c, "pgvector", ExtensionDefinition{Source: catalog.SourceAPT, Package: "postgresql-18-pgvector"})
			},
			want: "extensions.pgvector must be written as extensions.vector",
		},
		{
			name: "apt without package",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourceAPT})
			},
			want: "extensions.x.package is required",
		},
		{
			name: "preinstalled with package",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourcePreinstalled, Package: "postgresql-18-x"})
			},
			want: "extensions.x.package must be empty",
		},
		{
			name: "package injection",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourceAPT, Package: "postgresql-18-x;rm -rf /"})
			},
			want: "is not a Debian package name",
		},
		{
			name: "preload injection",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourcePreinstalled, Preload: "pg_cron,evil"})
			},
			want: "extensions.x.preload",
		},
		{
			name: "schema injection",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourcePreinstalled, Schema: "public; drop"})
			},
			want: "extensions.x.schema",
		},
		{
			name: "reserved setting",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"shared_preload_libraries": "evil"},
				})
			},
			want: "managed by vaire",
		},
		{
			name: "setting injection",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "safe\nlisten_addresses='*'"},
				})
			},
			want: "control character",
		},
		{
			name: "setting comment fragment",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on # comment"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "unquoted comma list",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "ddl,role"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "multiple bare tokens",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on off"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "assignment fragment",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "x=1"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "unquoted qualified token",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "foo.bar"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "unquoted unit",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "10MB"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "unquoted float",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "0.1"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "unquoted hyphen token",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "foo-bar"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "malformed signed number",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "--1"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "bare sign",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "+"},
				})
			},
			want: "not a safe PostgreSQL setting literal",
		},
		{
			name: "security logging setting",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"log_line_prefix": "%m"},
				})
			},
			want: "managed by vaire",
		},
		{
			name: "security logging copy setting",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled),
					CopySettings: map[string]string{"log_connections": "off"},
				})
			},
			want: "managed by vaire",
		},
		{
			name: "disabled copy safety without settings",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled)})
			},
			want: "copy_settings must suppress",
		},
		{
			name: "copy settings without disabled safety",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{
					Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetySafe),
					CopySettings: map[string]string{"x.mode": "off"},
				})
			},
			want: "copy_settings requires copy_safety disabled",
		},
		{
			name: "unknown copy safety",
			edit: func(c *Cluster) {
				addCustom(t, c, "x", ExtensionDefinition{Source: catalog.SourcePreinstalled, CopySafety: stringPtr("trust-me")})
			},
			want: "must be unknown, safe or disabled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extensionCluster(t)
			tc.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestHostScopeValidatesExtensionIntent(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"custom"}
	addCustom(t, &cfg, "custom", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"custom.mode": "ddl,role"},
	})
	if err := cfg.ValidateFor(ScopeHost); err == nil || !strings.Contains(err.Error(), "not a safe PostgreSQL setting literal") {
		t.Fatalf("host scope accepted invalid extension intent: %v", err)
	}

	cfg = extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"nope"}
	if err := cfg.ValidateFor(ScopeHost); err == nil || !strings.Contains(err.Error(), `"nope" is not built in`) {
		t.Fatalf("host scope accepted an unknown extension: %v", err)
	}
}

func TestUUIDOSSPCanonicalKeyAndSafeSQLExtensionNames(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"uuid-ossp"}
	addCustom(t, &cfg, "uuid-ossp", ExtensionDefinition{Source: catalog.SourcePreinstalled})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("uuid-ossp canonical key refused: %v", err)
	}
	if err := cfg.Set("extensions.uuid-ossp.ext_name", "uuid-ossp"); err != nil {
		t.Fatalf("dotted editing of uuid-ossp: %v", err)
	}
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[extensions.uuid-ossp]") {
		t.Fatalf("uuid-ossp key did not encode safely:\n%s", raw)
	}
	parsed, err := ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("uuid-ossp roundtrip: %v", err)
	}
	ext, err := parsed.ExtensionResolver().Resolve("uuid-ossp")
	if err != nil || ext.ExtName != "uuid-ossp" {
		t.Fatalf("uuid-ossp descriptor = %+v, %v", ext, err)
	}

	alias := extensionCluster(t)
	alias.Clusters[0].Extensions = []string{"uuid_ossp"}
	addCustom(t, &alias, "uuid_ossp", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "uuid-ossp",
	})
	if err := alias.Validate(); err != nil {
		t.Fatalf("uuid_ossp with ext_name uuid-ossp refused: %v", err)
	}
	aliasRaw, err := alias.Encode()
	if err != nil {
		t.Fatal(err)
	}
	aliasParsed, err := ParseCluster(aliasRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := aliasParsed.Validate(); err != nil {
		t.Fatalf("uuid_ossp descriptor TOML roundtrip: %v", err)
	}
	aliasExt, err := aliasParsed.ExtensionResolver().Resolve("uuid_ossp")
	if err != nil || aliasExt.ExtName != "uuid-ossp" {
		t.Fatalf("uuid_ossp descriptor = %+v, %v", aliasExt, err)
	}

	mixed := extensionCluster(t)
	mixed.Clusters[0].Extensions = []string{"mixed_ext"}
	addCustom(t, &mixed, "mixed_ext", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "Mixed.Ext!",
	})
	if err := mixed.Validate(); err != nil {
		t.Fatalf("quoted mixed-case extension name refused: %v", err)
	}
	ext, err = mixed.ExtensionResolver().Resolve("mixed_ext")
	if err != nil || ext.ExtName != "Mixed.Ext!" {
		t.Fatalf("mixed-case descriptor = %+v, %v", ext, err)
	}
}

func TestUnsafeExtensionNamesAreRefused(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		extName string
	}{
		{name: "path traversal", key: "bad_ext", extName: "../evil"},
		{name: "parent segment", key: "bad_ext", extName: "foo..bar"},
		{name: "dot", key: "bad_ext", extName: "."},
		{name: "forward slash", key: "bad_ext", extName: "foo/bar"},
		{name: "backslash", key: "bad_ext", extName: `foo\bar`},
		{name: "double hyphen", key: "bad_ext", extName: "foo--bar"},
		{name: "leading hyphen", key: "bad_ext", extName: "-foo"},
		{name: "trailing hyphen", key: "bad_ext", extName: "foo-"},
		{name: "control", key: "bad_ext", extName: "foo\nbar"},
		{name: "too long", key: "bad_ext", extName: strings.Repeat("a", 64)},
		{name: "bad canonical key", key: "bad--key", extName: "safe_ext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extensionCluster(t)
			cfg.Clusters[0].Extensions = []string{tc.key}
			addCustom(t, &cfg, tc.key, ExtensionDefinition{
				Source: catalog.SourcePreinstalled, ExtName: tc.extName,
			})
			if err := cfg.Validate(); err == nil {
				t.Fatalf("unsafe name %q/%q passed", tc.key, tc.extName)
			}
		})
	}
}

func TestCustomOmittedSafetyIsUnknownAndNotStandbySafe(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"custom"}
	addCustom(t, &cfg, "custom", ExtensionDefinition{Source: catalog.SourcePreinstalled})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a primary-only custom definition was refused: %v", err)
	}
	ext, err := cfg.ExtensionResolver().Resolve("custom")
	if err != nil {
		t.Fatal(err)
	}
	if ext.CopySafety != catalog.CopySafetyUnknown {
		t.Fatalf("omitted copy_safety = %q", ext.CopySafety)
	}
	if ext.StandbySafe {
		t.Fatal("omitted standby_safe became safe")
	}
	if len(ext.CopySettings) != 0 {
		t.Fatalf("omitted copy_settings = %v", ext.CopySettings)
	}
}

func TestBuiltinDescriptorsPassSafeLiterals(t *testing.T) {
	cfg := extensionCluster(t)
	for _, name := range catalog.Names() {
		if _, err := cfg.ExtensionResolver().Resolve(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExtensionSettingSafeLiterals(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"custom"}
	addCustom(t, &cfg, "custom", ExtensionDefinition{
		Source: catalog.SourcePreinstalled,
		Settings: map[string]string{
			"custom.mode":      "'on'",
			"custom.path":      "'public, ext'",
			"custom.size":      "'10MB'",
			"custom.ratio":     "'0.1'",
			"custom.count":     "-1",
			"custom.plus_int":  "+1",
			"custom.list":      "'ddl,role'",
			"custom.qualified": "'foo.bar'",
			"custom.plus":      "'+1'",
			"custom.comment":   "'on # comment'",
			"custom.quoted":    "'it''s'",
		},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("safe setting literals refused: %v", err)
	}
}

func TestProviderOverrideRenewsSafety(t *testing.T) {
	base := extensionCluster(t)
	base.Clusters[0].Extensions = []string{"vector"}
	override := func(c *Cluster, def ExtensionDefinition) {
		addCustom(t, c, "vector", def)
	}

	cfg := base
	override(&cfg, ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-pgvector-alternate",
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "copy_safety must be renewed") || !strings.Contains(err.Error(), "standby_safe must be renewed") {
		t.Fatalf("provider override without renewed safety: %v", err)
	}

	cfg = base
	override(&cfg, ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-pgvector-alternate",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("renewed provider override refused: %v", err)
	}
	ext, err := cfg.ExtensionResolver().Resolve("vector")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Package != "postgresql-18-pgvector-alternate" || ext.CopySafety != catalog.CopySafetySafe || !ext.StandbySafe {
		t.Fatalf("override not resolved: %+v", ext)
	}

	cfg = base
	override(&cfg, ExtensionDefinition{
		Source:     catalog.SourcePreinstalled,
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("preinstalled override refused: %v", err)
	}
	ext, _ = cfg.ExtensionResolver().Resolve("vector")
	if ext.Source != catalog.SourcePreinstalled || ext.Package != "" {
		t.Fatalf("preinstalled override kept an APT package: %+v", ext)
	}

	cfg = extensionCluster(t)
	addCustom(t, &cfg, "auto_explain", ExtensionDefinition{Preload: "something_else"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "preload is managed by vaire") {
		t.Fatalf("auto_explain preload override: %v", err)
	}
	cfg = extensionCluster(t)
	addCustom(t, &cfg, "auto_explain", ExtensionDefinition{ExtName: "auto_explain"})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cannot change built-in") {
		t.Fatalf("auto_explain ext_name override: %v", err)
	}

	cfg = extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"pg_stat_statements"}
	addCustom(t, &cfg, "pg_stat_statements", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default module provider override refused: %v", err)
	}
	ext, _ = cfg.ExtensionResolver().Resolve("pg_stat_statements")
	if !ext.Default || ext.Preload != "pg_stat_statements" {
		t.Fatalf("default module invariants lost: %+v", ext)
	}
}

func TestExtensionHostConflicts(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters = append(cfg.Clusters, ClusterSpec{
		Name: "other", Node: "db1", Port: 5434, Stanza: "other-def456", Size: "small", StatementTimeout: "60s",
		Extensions: []string{"a_same", "a_cron"},
	})
	cfg.Clusters[0].Extensions = []string{"a_same", "a_cron"}
	addCustom(t, &cfg, "a_same", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-a", ExtName: "same",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	addCustom(t, &cfg, "a_cron", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		Settings: map[string]string{"cron.max_running_jobs": "1"},
	})
	cfg.Clusters[1].Extensions = []string{"b_same", "b_cron"}
	addCustom(t, &cfg, "b_same", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-b", ExtName: "same",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	addCustom(t, &cfg, "b_cron", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		Settings: map[string]string{"cron.max_running_jobs": "2"},
	})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("host conflicts passed validation")
	}
	if !strings.Contains(err.Error(), `resolve SQL extension "same" with conflicting providers`) {
		t.Errorf("missing SQL extension conflict: %v", err)
	}
	if !strings.Contains(err.Error(), `load library "pg_cron" with conflicting providers`) {
		t.Errorf("missing library conflict: %v", err)
	}
}

func TestExtensionSettingUnionConflicts(t *testing.T) {
	base := extensionCluster(t)
	base.Clusters[0].Extensions = []string{"one", "two"}
	fresh := func() Cluster {
		cfg := base.Clone()
		cfg.Clusters[0].Extensions = []string{"one", "two"}
		return cfg
	}
	add := func(cfg *Cluster, one, two ExtensionDefinition) {
		addCustom(t, cfg, "one", one)
		addCustom(t, cfg, "two", two)
	}

	cfg := fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "off"}})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `setting "x.mode" conflicts`) || !strings.Contains(err.Error(), "normal union") {
		t.Fatalf("normal setting conflict: %v", err)
	}

	cfg = fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("equal values should be allowed: %v", err)
	}

	// The union compares effective per-database values, so {db} and the
	// database's literal name are the same requirement.
	cfg = fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"cron.database_name": "'{db}'"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"cron.database_name": "'app'"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("equal effective setting values refused: %v", err)
	}

	cfg = fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "'on'"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("quoted and unquoted equal values refused: %v", err)
	}

	// Different extension and preload identities do not make the setting
	// key independent: the renderer still emits one line for it.
	cfg = fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Preload: "one_lib", Settings: map[string]string{"x.mode": "on"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Preload: "two_lib", Settings: map[string]string{"x.mode": "off"}})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `setting "x.mode" conflicts`) {
		t.Fatalf("cross-identity setting conflict: %v", err)
	}

	// One descriptor may suppress its own normal setting in a copy.
	cfg = fresh()
	cfg.Clusters[0].Extensions = []string{"one"}
	addCustom(t, &cfg, "one", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"},
		CopySafety: stringPtr(catalog.CopySafetyDisabled), CopySettings: map[string]string{"x.mode": "off"},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("same-descriptor copy suppression refused: %v", err)
	}

	// Two descriptors' copy requirements must agree.
	cfg = fresh()
	add(&cfg, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled),
		CopySettings: map[string]string{"x.mode": "off"},
	}, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled),
		CopySettings: map[string]string{"x.mode": "on"},
	})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "copy union") {
		t.Fatalf("multiple copy requirements conflict: %v", err)
	}

	// A copy suppression must also agree with another descriptor's normal
	// requirement, even though the two normal values agree.
	cfg = fresh()
	add(&cfg, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"},
		CopySafety: stringPtr(catalog.CopySafetyDisabled), CopySettings: map[string]string{"x.mode": "off"},
	}, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"},
	})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "copy union") {
		t.Fatalf("copy suppression versus another normal value: %v", err)
	}
}

func TestRecoveryExtensionSettingUnionConflicts(t *testing.T) {
	base := extensionCluster(t)
	recovery := []string{"one", "two"}
	base.Clusters[0].RecoveryExtensions = &recovery
	fresh := func() Cluster {
		cfg := base.Clone()
		list := []string{"one", "two"}
		cfg.Clusters[0].RecoveryExtensions = &list
		return cfg
	}
	add := func(cfg *Cluster, one, two ExtensionDefinition) {
		addCustom(t, cfg, "one", one)
		addCustom(t, cfg, "two", two)
	}

	cfg := fresh()
	add(&cfg, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "one_lib",
		Settings: map[string]string{"x.mode": "on"},
	}, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "two_lib",
		Settings: map[string]string{"x.mode": "off"},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `clusters[0].recovery_extensions setting "x.mode" conflicts`) ||
		!strings.Contains(err.Error(), "normal union") {
		t.Fatalf("recovery normal setting conflict: %v", err)
	}

	cfg = fresh()
	add(&cfg, ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"}},
		ExtensionDefinition{Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "'on'"}})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("equal quoted recovery values refused: %v", err)
	}

	cfg = fresh()
	cfg.Clusters[0].RecoveryExtensions = &[]string{"one"}
	addCustom(t, &cfg, "one", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"x.mode": "on"},
		CopySafety: stringPtr(catalog.CopySafetyDisabled), CopySettings: map[string]string{"x.mode": "off"},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("same-descriptor recovery copy suppression refused: %v", err)
	}

	cfg = fresh()
	add(&cfg, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled),
		CopySettings: map[string]string{"x.mode": "off"},
	}, ExtensionDefinition{
		Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyDisabled),
		CopySettings: map[string]string{"x.mode": "on"},
	})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "copy union") {
		t.Fatalf("recovery copy requirements conflict: %v", err)
	}

	// The implicit default modules are part of a recovery selection.
	cfg = extensionCluster(t)
	cfg.Clusters[0].RecoveryExtensions = &[]string{"one"}
	addCustom(t, &cfg, "one", ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Settings: map[string]string{"pg_stat_statements.track": "all"},
	})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `clusters[0].recovery_extensions setting "pg_stat_statements.track" conflicts`) {
		t.Fatalf("recovery implicit-default conflict: %v", err)
	}
}

func TestHostProviderValidationIncludesImplicitDefaults(t *testing.T) {
	addDefaults := func(cfg *Cluster) {
		addCustom(t, cfg, "pg_stat_statements", ExtensionDefinition{
			Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements-a",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		})
	}
	addCustomStats := func(cfg *Cluster, pkg string) {
		addCustom(t, cfg, "custom_stats", ExtensionDefinition{
			Source: catalog.SourceAPT, Package: pkg, ExtName: "pg_stat_statements",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		})
	}

	// The implicit default provider is part of the effective selection even
	// when the database names only custom_stats.
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"custom_stats"}
	addDefaults(&cfg)
	addCustomStats(&cfg, "postgresql-18-pg-stat-statements-b")
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "pg_stat_statements" with conflicting providers`) ||
		!strings.Contains(err.Error(), "postgresql-18-pg-stat-statements-a") ||
		!strings.Contains(err.Error(), "postgresql-18-pg-stat-statements-b") {
		t.Fatalf("implicit default provider conflict: %v", err)
	}

	// The same provider selection is compatible even though custom_stats
	// omits the default module's preload and settings.
	cfg = extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"custom_stats"}
	addDefaults(&cfg)
	addCustomStats(&cfg, "postgresql-18-pg-stat-statements-a")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("compatible implicit provider selection refused: %v", err)
	}
	packages, err := cfg.ExtensionResolver().Packages([]string{"custom_stats"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"postgresql-18-pg-stat-statements-a"}) {
		t.Fatalf("compatible package union = %v", packages)
	}

	// The same conflict is detected across databases when one database
	// relies on the implicit default and another names custom_stats.
	cfg = extensionCluster(t)
	cfg.Clusters = append(cfg.Clusters, ClusterSpec{
		Name: "other", Node: "db1", Port: 5434, Stanza: "other-def456", Size: "small",
		StatementTimeout: "60s", Extensions: []string{"custom_stats"},
	})
	cfg.Clusters[0].Extensions = nil
	addDefaults(&cfg)
	addCustomStats(&cfg, "postgresql-18-pg-stat-statements-b")
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "pg_stat_statements" with conflicting providers`) {
		t.Fatalf("cross-database implicit default provider conflict: %v", err)
	}
}

func TestHostAggregateSameSQLMetadata(t *testing.T) {
	def := sharedSQLDefinition
	t.Run("baseline defaults remain compatible", func(t *testing.T) {
		cfg := extensionCluster(t)
		cfg.Clusters = append(cfg.Clusters, hostClusterSpec("other", 5434))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("baseline host refused: %v", err)
		}
		packages, err := cfg.ExtensionResolver().Packages(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(packages) != 0 {
			t.Fatalf("baseline package union = %v", packages)
		}
	})

	t.Run("different preloads and settings across databases", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("one_lib", map[string]string{"shared.mode": "on"}),
			"two": def("two_lib", map[string]string{"shared.mode": "off"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		err := cfg.Validate()
		if err == nil {
			t.Fatal("conflicting same-SQL metadata passed")
		}
		for _, want := range []string{
			`resolve SQL extension "shared" with conflicting preload libraries`,
			`resolve SQL extension "shared" with conflicting setting "shared.mode"`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q: %v", want, err)
			}
		}
	})

	t.Run("same preload conflicting settings across databases", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
			"two": def("shared_lib", map[string]string{"shared.mode": "off"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting setting "shared.mode"`) {
			t.Fatalf("conflicting settings passed: %v", err)
		}
	})

	t.Run("different preloads with equal normalized settings", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("one_lib", map[string]string{"shared.mode": "on"}),
			"two": def("two_lib", map[string]string{"shared.mode": "'on'"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting preload libraries`) {
			t.Fatalf("conflicting preloads passed: %v", err)
		}
		if strings.Contains(err.Error(), `conflicting setting "shared.mode"`) {
			t.Fatalf("equal normalized settings were reported as a conflict: %v", err)
		}
	})

	t.Run("different preloads with disjoint settings", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("one_lib", map[string]string{"shared.one": "on"}),
			"two": def("two_lib", map[string]string{"shared.two": "off"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting preload libraries`) {
			t.Fatalf("distinct preloads passed: %v", err)
		}
	})

	t.Run("sparse duplicate omits preload and settings", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("", nil),
			"two": def("shared_lib", map[string]string{"shared.mode": "on"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("compatible sparse metadata refused: %v", err)
		}
	})

	t.Run("same preload sparse overlapping equal settings", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
			"two": def("shared_lib", map[string]string{"shared.mode": "'on'", "shared.extra": "off"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("compatible overlapping settings refused: %v", err)
		}
	})

	t.Run("sparse first does not mask a later preload conflict", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"sparse": def("", nil),
			"rich":   def("one_lib", map[string]string{"shared.mode": "on"}),
			"other":  def("two_lib", map[string]string{"shared.mode": "off"}),
		}, hostClusterSpec("app", 5433, "sparse"), hostClusterSpec("rich", 5434, "rich"), hostClusterSpec("third", 5435, "other"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting preload libraries`) {
			t.Fatalf("sparse first masked a preload conflict: %v", err)
		}
	})

	t.Run("sparse first does not mask a later setting conflict", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"sparse": def("", nil),
			"on":     def("", map[string]string{"shared.mode": "on"}),
			"off":    def("", map[string]string{"shared.mode": "off"}),
		}, hostClusterSpec("app", 5433, "sparse"), hostClusterSpec("on", 5434, "on"), hostClusterSpec("off", 5435, "off"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting setting "shared.mode"`) {
			t.Fatalf("sparse first masked a setting conflict: %v", err)
		}
	})

	t.Run("rich first then sparse does not mask a later conflict", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"rich":   def("one_lib", map[string]string{"shared.mode": "on"}),
			"sparse": def("", nil),
			"other":  def("two_lib", map[string]string{"shared.mode": "off"}),
		}, hostClusterSpec("rich", 5433, "rich"), hostClusterSpec("app", 5434, "sparse"), hostClusterSpec("third", 5435, "other"))
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting preload libraries`) {
			t.Fatalf("rich first masked a preload conflict: %v", err)
		}
	})

	t.Run("sparse first still accumulates compatible requirements", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"sparse": def("", nil),
			"rich":   def("shared_lib", map[string]string{"shared.mode": "on"}),
			"equal":  def("shared_lib", map[string]string{"shared.mode": "'on'", "shared.extra": "off"}),
		}, hostClusterSpec("app", 5433, "sparse"), hostClusterSpec("rich", 5434, "rich"), hostClusterSpec("equal", 5435, "equal"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("compatible accumulated requirements refused: %v", err)
		}
	})

	t.Run("rich first then sparse remains compatible", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"sparse": def("", nil),
			"rich":   def("shared_lib", map[string]string{"shared.mode": "on"}),
		}, hostClusterSpec("rich", 5433, "rich"), hostClusterSpec("app", 5434, "sparse"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("rich then sparse refused: %v", err)
		}
	})
}

func TestHostAggregateCurrentRecoveryMetadata(t *testing.T) {
	def := sharedSQLDefinition
	t.Run("current and recovery conflicting preloads and settings", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		recovery := []string{"two"}
		db.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("one_lib", map[string]string{"shared.mode": "on"}),
			"two": def("two_lib", map[string]string{"shared.mode": "off"}),
		}, db)
		err := cfg.Validate()
		if err == nil {
			t.Fatal("conflicting current/recovery metadata passed")
		}
		for _, want := range []string{
			`clusters.app.recovery_extensions "two"`,
			`resolve SQL extension "shared" with conflicting preload libraries`,
			`resolve SQL extension "shared" with conflicting setting "shared.mode"`,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q: %v", want, err)
			}
		}
	})

	t.Run("current and recovery same preload conflicting settings", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		recovery := []string{"two"}
		db.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
			"two": def("shared_lib", map[string]string{"shared.mode": "off"}),
		}, db)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting setting "shared.mode"`) {
			t.Fatalf("conflicting current/recovery settings passed: %v", err)
		}
	})

	t.Run("recovery metadata is checked across databases", func(t *testing.T) {
		first := hostClusterSpec("app", 5433, "one")
		second := hostClusterSpec("other", 5434)
		recovery := []string{"two"}
		second.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("one_lib", map[string]string{"shared.mode": "on"}),
			"two": def("two_lib", map[string]string{"shared.mode": "off"}),
		}, first, second)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), `resolve SQL extension "shared" with conflicting preload libraries`) {
			t.Fatalf("cross-database recovery metadata passed: %v", err)
		}
	})

	t.Run("sparse recovery accumulates against a rich current selection", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		recovery := []string{"two"}
		db.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
			"two": def("", nil),
		}, db)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("sparse recovery refused: %v", err)
		}
	})

	t.Run("rich recovery accumulates against a sparse current selection", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		recovery := []string{"two"}
		db.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("", nil),
			"two": def("shared_lib", map[string]string{"shared.mode": "on"}),
		}, db)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("rich recovery refused: %v", err)
		}
	})

	t.Run("same descriptor in current and recovery", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		recovery := []string{"one"}
		db.RecoveryExtensions = &recovery
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
		}, db)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("identical current/recovery selection refused: %v", err)
		}
	})

	t.Run("explicitly empty recovery still adds defaults only", func(t *testing.T) {
		db := hostClusterSpec("app", 5433, "one")
		empty := []string{}
		db.RecoveryExtensions = &empty
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def("shared_lib", map[string]string{"shared.mode": "on"}),
		}, db)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("explicitly empty recovery refused: %v", err)
		}
	})
}

func TestHostAggregateDatabasePlaceholderIsNotFalseConflict(t *testing.T) {
	def := func(settings map[string]string) ExtensionDefinition {
		return sharedSQLDefinition("shared_lib", settings)
	}
	t.Run("same descriptor across databases", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def(map[string]string{"shared.database": "'{db}'"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "one"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("per-database substitution falsely conflicted: %v", err)
		}
	})
	t.Run("two descriptors with the same placeholder requirement", func(t *testing.T) {
		cfg := hostExtensionCluster(t, map[string]ExtensionDefinition{
			"one": def(map[string]string{"shared.database": "'{db}'"}),
			"two": def(map[string]string{"shared.database": "'{db}'"}),
		}, hostClusterSpec("app", 5433, "one"), hostClusterSpec("other", 5434, "two"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("equal placeholder requirements falsely conflicted: %v", err)
		}
	})
}

func TestDuplicateSQLExtensionNamesWithinDatabase(t *testing.T) {
	cfg := extensionCluster(t)
	cfg.Clusters[0].Extensions = []string{"one", "two"}
	addCustom(t, &cfg, "one", ExtensionDefinition{Source: catalog.SourcePreinstalled, ExtName: "same"})
	addCustom(t, &cfg, "two", ExtensionDefinition{Source: catalog.SourcePreinstalled, ExtName: "same"})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `both create SQL extension "same"`) {
		t.Fatalf("duplicate SQL extension names: %v", err)
	}
}

func TestRecoveryExtensionsEditingAndClone(t *testing.T) {
	cfg := extensionCluster(t)
	value, err := cfg.Get("clusters.app.recovery_extensions")
	if err != nil {
		t.Fatal(err)
	}
	if value != nil {
		t.Fatalf("absent recovery_extensions = %v", value)
	}
	if err := cfg.Set("clusters.app.recovery_extensions", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters[0].RecoveryExtensions == nil || len(*cfg.Clusters[0].RecoveryExtensions) != 0 {
		t.Fatalf("explicit empty set = %v", cfg.Clusters[0].RecoveryExtensions)
	}
	value, err = cfg.Get("clusters.app.recovery_extensions")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := value.([]string); !ok || got == nil || len(got) != 0 {
		t.Fatalf("explicit empty get = %#v", value)
	}
	if err := cfg.Set("clusters.app.recovery_extensions", `["pg_trgm", "vector"]`); err != nil {
		t.Fatal(err)
	}
	if got := *cfg.Clusters[0].RecoveryExtensions; !slices.Equal(got, []string{"pg_trgm", "vector"}) {
		t.Fatalf("populated set = %v", got)
	}
	if err := cfg.Set("clusters.app.recovery_extensions", "["); err == nil {
		t.Fatal("a malformed list was accepted")
	}
	if got := *cfg.Clusters[0].RecoveryExtensions; !slices.Equal(got, []string{"pg_trgm", "vector"}) {
		t.Fatalf("a failed set changed the value: %v", got)
	}

	addCustom(t, &cfg, "custom", ExtensionDefinition{
		Source:   catalog.SourcePreinstalled,
		Settings: map[string]string{"custom.mode": "on"},
	})
	cfg.Clusters[0].Extensions = []string{"custom"}
	clone := cfg.Clone()
	clone.Extensions["custom"].Settings["custom.mode"] = "off"
	clone.Clusters[0].RecoveryExtensions = &[]string{"pg_trgm"}
	if cfg.Extensions["custom"].Settings["custom.mode"] != "on" {
		t.Fatal("Cluster.Clone shares definition maps")
	}
	if slices.Equal(*cfg.Clusters[0].RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("Cluster.Clone shares recovery slices")
	}
}

func TestExtensionResolverPreloadsPackagesAndAliases(t *testing.T) {
	cfg := extensionCluster(t)
	addCustom(t, &cfg, "zzz", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-zzz", Preload: "zzz_lib",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	resolver := cfg.ExtensionResolver()
	if canonical, ok := resolver.Canonical("pgvector"); !ok || canonical != "vector" {
		t.Fatalf("alias = %q %v", canonical, ok)
	}
	effective, err := resolver.EffectiveNames([]string{"pg_stat_statements", "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(effective, []string{"pg_stat_statements", "auto_explain", "zzz"}) {
		t.Fatalf("effective selection = %v", effective)
	}
	enabled, err := resolver.Enabled([]string{"pgvector", "vector", "zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 2 || enabled[0].Name != "vector" || enabled[1].Name != "zzz" {
		t.Fatalf("enabled = %+v", enabled)
	}
	preloads, err := resolver.Preloads([]string{"zzz", "pg_cron", "pg_stat_statements"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preloads, []string{"pg_stat_statements", "auto_explain", "pg_cron", "zzz_lib"}) {
		t.Fatalf("preloads = %v", preloads)
	}
	packages, err := resolver.Packages([]string{"zzz", "vector", "pg_trgm"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"postgresql-18-pgvector", "postgresql-18-zzz"}) {
		t.Fatalf("packages = %v", packages)
	}
	changed, err := resolver.PreloadChanges([]string{"pg_stat_statements"}, []string{"pg_stat_statements", "zzz"})
	if err != nil || !changed {
		t.Fatalf("preload change = %v, %v", changed, err)
	}
	changed, err = resolver.PreloadChanges([]string{"vector"}, []string{"vector", "pg_trgm"})
	if err != nil || changed {
		t.Fatalf("non-preload change = %v, %v", changed, err)
	}
}

func TestPackagesIncludeImplicitDefaultProviders(t *testing.T) {
	cfg := extensionCluster(t)
	resolver := cfg.ExtensionResolver()
	empty, err := resolver.Packages(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unchanged baseline package union = %v, %v", empty, err)
	}
	addCustom(t, &cfg, "auto_explain", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	addCustom(t, &cfg, "pg_stat_statements", ExtensionDefinition{
		Source: catalog.SourceAPT, Package: "postgresql-18-pg-stat-statements",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
	})
	resolver = cfg.ExtensionResolver()
	packages, err := resolver.Packages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"postgresql-18-auto-explain", "postgresql-18-pg-stat-statements"}) {
		t.Fatalf("implicit default packages = %v", packages)
	}
	packages, err = resolver.Packages([]string{"vector"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"postgresql-18-auto-explain", "postgresql-18-pg-stat-statements", "postgresql-18-pgvector"}) {
		t.Fatalf("effective packages = %v", packages)
	}
}

func TestConfigSetMapOfStructEditing(t *testing.T) {
	cfg := extensionCluster(t)
	if err := cfg.Set("extensions.new.source", "preinstalled"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("extensions.new.settings.foo.bar", "baz"); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Get("extensions.new.settings.foo.bar"); err != nil || got != "baz" {
		t.Fatalf("dotted setting map key = %v, %v", got, err)
	}
	if err := cfg.Set("extensions.new.ext_name", "renamed"); err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Get("extensions.new.ext_name"); err != nil || got != "renamed" {
		t.Fatalf("map-of-struct edit = %v, %v", got, err)
	}
	if err := cfg.Set("extensions", "x"); err == nil {
		t.Fatal("setting the extensions table from the command line was accepted")
	}
	if err := cfg.Set("extensions.new", "x"); err == nil {
		t.Fatal("setting a whole descriptor from the command line was accepted")
	}
}

func TestRoutingViewDeepCopiesExtensionMetadata(t *testing.T) {
	cfg := extensionCluster(t)
	addCustom(t, &cfg, "custom", ExtensionDefinition{
		Source:   catalog.SourcePreinstalled,
		Settings: map[string]string{"custom.mode": "on"},
	})
	cfg.Clusters[0].Extensions = []string{"custom"}
	cfg.Clusters[0].RecoveryExtensions = &[]string{"pg_trgm"}
	view := cfg.RoutingView(nil, "")
	view.Extensions["custom"].Settings["custom.mode"] = "off"
	*view.Clusters[0].RecoveryExtensions = []string{"vector"}
	if cfg.Extensions["custom"].Settings["custom.mode"] != "on" {
		t.Fatal("RoutingView shares extension settings")
	}
	if !slices.Equal(*cfg.Clusters[0].RecoveryExtensions, []string{"pg_trgm"}) {
		t.Fatal("RoutingView shares recovery slices")
	}
}

func TestExtensionIntentChanges(t *testing.T) {
	cfg := extensionCluster(t)
	addCustom(t, &cfg, "custom", ExtensionDefinition{
		Source:   catalog.SourcePreinstalled,
		Settings: map[string]string{"custom.mode": "on"},
	})
	cfg.Clusters[0].Extensions = []string{"custom"}
	if changes := ExtensionIntentChanges(cfg, cfg.Clone()); len(changes) != 0 {
		t.Fatalf("identical config changed: %v", changes)
	}

	noteOnly := cfg.Clone()
	noteOnly.Extensions["custom"] = func() ExtensionDefinition {
		d := noteOnly.Extensions["custom"]
		d.Note = "human note"
		return d
	}()
	if changes := ExtensionIntentChanges(cfg, noteOnly); len(changes) != 0 {
		t.Fatalf("a note changed runtime intent: %v", changes)
	}

	settings := cfg.Clone()
	settings.Extensions["custom"] = func() ExtensionDefinition {
		d := settings.Extensions["custom"]
		d.Settings = map[string]string{"custom.mode": "off"}
		return d
	}()
	if changes := ExtensionIntentChanges(cfg, settings); !slices.Equal(changes, []string{"extensions.custom"}) {
		t.Fatalf("settings change = %v", changes)
	}

	unused := cfg.Clone()
	addCustom(t, &unused, "unused", ExtensionDefinition{Source: catalog.SourcePreinstalled})
	if changes := ExtensionIntentChanges(cfg, unused); len(changes) != 0 {
		t.Fatalf("an unused definition changed intent: %v", changes)
	}

	empty := cfg.Clone()
	empty.Clusters[0].RecoveryExtensions = &[]string{}
	if changes := ExtensionIntentChanges(cfg, empty); !slices.Equal(changes, []string{"clusters.app.recovery_extensions"}) {
		t.Fatalf("absence to explicit empty = %v", changes)
	}
	populated := empty.Clone()
	populated.Clusters[0].RecoveryExtensions = &[]string{"pg_trgm"}
	if changes := ExtensionIntentChanges(empty, populated); !slices.Equal(changes, []string{"clusters.app.recovery_extensions"}) {
		t.Fatalf("empty to populated = %v", changes)
	}

	added := cfg.Clone()
	added.Clusters[0].Extensions = []string{"custom", "pg_trgm"}
	if changes := ExtensionIntentChanges(cfg, added); !slices.Equal(changes, []string{"clusters.app.extensions"}) {
		t.Fatalf("extension list = %v", changes)
	}

	defaults := cfg.Clone()
	addCustom(t, &defaults, "auto_explain", ExtensionDefinition{
		Source:   catalog.SourcePreinstalled,
		Settings: map[string]string{"auto_explain.log_level": "info"},
	})
	if changes := ExtensionIntentChanges(cfg, defaults); !slices.Equal(changes, []string{"extensions.auto_explain"}) {
		t.Fatalf("default module descriptor = %v", changes)
	}
}

func TestExtensionDefinitionStrictDecoding(t *testing.T) {
	cfg := extensionCluster(t)
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n[extensions.foo]\nsource = \"apt\"\npackage = \"postgresql-18-foo\"\nshell = \"rm -rf /\"\n")...)
	if _, err := ParseCluster(raw); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Fatalf("descriptor with a shell hook parsed: %v", err)
	}
}

func TestExtensionDefinitionEncodeIsDeterministic(t *testing.T) {
	cfg := extensionCluster(t)
	addCustom(t, &cfg, "zzz", ExtensionDefinition{Source: catalog.SourcePreinstalled})
	addCustom(t, &cfg, "aaa", ExtensionDefinition{Source: catalog.SourcePreinstalled})
	first, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	second, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("definition map encoding is not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestBaselineEncodeOmitsNewTables(t *testing.T) {
	cfg := extensionCluster(t)
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "[extensions") || strings.Contains(string(raw), "recovery_extensions") {
		t.Fatalf("baseline TOML gained new sections:\n%s", raw)
	}
}
