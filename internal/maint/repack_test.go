package maint

import (
	"strings"
	"testing"
)

func TestRepackArgs(t *testing.T) {
	got, err := RepackArgs("/var/run/postgresql", 5433, "app", "public.events")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-u", "postgres", "--", RepackBin, "-h", "/var/run/postgresql", "-p", "5433",
		"-U", "postgres", "-d", "app", "--table", `"public"."events"`, "--no-kill-backend", "--wait-timeout=60"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %v", got)
	}
	for _, part := range got {
		if part == "--no-superuser-check" || part == "--password" || strings.Contains(part, "PGPASSWORD") {
			t.Errorf("unsafe argv part %q", part)
		}
	}
	got, err = RepackArgs("/s", 5433, "app", "Sales.Orders")
	if err != nil || !strings.Contains(strings.Join(got, " "), `--table "Sales"."Orders"`) {
		t.Fatalf("quoted table: %v %v", got, err)
	}
	if _, err := RepackArgs("/s", 0, "app", "events"); err == nil {
		t.Error("invalid port passed")
	}
	if _, err := RepackArgs("/s", 5433, "app", "bad;name"); err == nil {
		t.Error("invalid table passed")
	}
}

func TestRepackCandidates(t *testing.T) {
	r := BloatReport{Rows: []BloatRow{
		{Kind: "table", Schema: "public", Table: "small", BloatBytes: 10 << 20, Ratio: 0.9},
		{Kind: "index", Schema: "public", Table: "big", Index: "big_pkey", BloatBytes: 900 << 20, Ratio: 0.9},
		{Kind: "table", Schema: "public", Table: "big", BloatBytes: 500 << 20, Ratio: 0.5},
		{Kind: "table", Schema: "app", Table: "mid", BloatBytes: 100 << 20, Ratio: 0.4},
		{Kind: "table", Schema: "public", Table: "lean", BloatBytes: 800 << 20, Ratio: 0.2},
		{Kind: "table", Schema: "public", Table: "tie", BloatBytes: 100 << 20, Ratio: 0.4},
	}}
	if got := RepackCandidates(r, 0.4, 1); strings.Join(got, ",") != "public.big,app.mid,public.tie,public.small" {
		t.Fatalf("by hand: %v", got)
	}
	if got := RepackCandidates(r, 0.4, AutoRepackMinBytes); strings.Join(got, ",") != "public.big,app.mid,public.tie" {
		t.Fatalf("auto: %v", got)
	}
	if got := RepackCandidates(r, 0.4, 1<<40); len(got) != 0 {
		t.Fatalf("min bytes: %v", got)
	}
}

func TestRepackHeavyAndOptions(t *testing.T) {
	if !(Options{Kind: Repack}).Heavy() {
		t.Fatal("a repack takes the heavy-I/O lock")
	}
	if (Options{Kind: Analyze}).Heavy() || (Options{Kind: Vacuum, Table: "t"}).Heavy() {
		t.Error("ordinary table maintenance became heavy")
	}
	o := Options{Kind: Repack, DB: "app", Table: "public.events", Now: true}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRepackKeySQL(t *testing.T) {
	q, _ := QuoteRelation("public.events")
	sql := RepackKeySQL(q)
	for _, want := range []string{"indisunique", "indisvalid", "indpred IS NULL", "indexprs IS NULL",
		"0 <> ALL(i.indkey)", "indnkeyatts", "attnotnull", `'"public"."events"'::regclass`} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	if strings.Contains(sql, "indnatts") {
		t.Error("key SQL checks included columns instead of key attributes")
	}
}

func TestRelationFactsAcceptLeavesAndRejectUnsupported(t *testing.T) {
	ordinary := RelationFacts{RelationExists: true, Schema: "public", Table: "events", Kind: "r", Persistence: "p", AccessMethod: "heap"}
	if reason, _, ok := ordinary.UnsupportedOrdinaryTable(); ok {
		t.Fatalf("ordinary rejected: %s", reason)
	}
	leaf := ordinary
	leaf.IsPartition, leaf.HasParent = true, true
	if reason, _, ok := leaf.UnsupportedOrdinaryTable(); ok {
		t.Fatalf("partition leaf rejected: %s", reason)
	}
	child := ordinary
	child.HasParent = true
	if reason, _, ok := child.UnsupportedOrdinaryTable(); ok {
		t.Fatalf("inheritance child rejected: %s", reason)
	}
	for _, tc := range []struct {
		name string
		f    RelationFacts
		want string
	}{
		{"partition parent", RelationFacts{RelationExists: true, Schema: "public", Table: "p", Kind: "p", Persistence: "p", AccessMethod: "heap"}, RepackReasonPartitionParent},
		{"foreign", RelationFacts{RelationExists: true, Schema: "public", Table: "f", Kind: "f", Persistence: "p"}, RepackReasonForeignTable},
		{"matview", RelationFacts{RelationExists: true, Schema: "public", Table: "m", Kind: "m", Persistence: "p", AccessMethod: "heap"}, RepackReasonUnsupportedRelation},
		{"unlogged", RelationFacts{RelationExists: true, Schema: "public", Table: "u", Kind: "r", Persistence: "u", AccessMethod: "heap"}, RepackReasonPersistence},
		{"non-heap", RelationFacts{RelationExists: true, Schema: "public", Table: "c", Kind: "r", Persistence: "p", AccessMethod: "columnar"}, RepackReasonAccessMethod},
		{"system", RelationFacts{RelationExists: true, Schema: "pg_catalog", Table: "x", Kind: "r", Persistence: "p", AccessMethod: "heap", SystemSchema: true}, RepackReasonSystemRelation},
		{"missing", RelationFacts{}, RepackReasonRelationNotFound},
	} {
		reason, _, ok := tc.f.UnsupportedOrdinaryTable()
		if !ok || reason != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.name, reason, ok, tc.want)
		}
	}
}

func TestRelationSQLUsesScopedExtensionChain(t *testing.T) {
	q, _ := QuoteRelation("public.events")
	facts := RelationFactsSQL(q)
	for _, want := range []string{"relkind", "relpersistence", "am.amname", "relispartition", "pg_inherits", "to_regclass"} {
		if !strings.Contains(facts, want) {
			t.Errorf("relation facts missing %q", want)
		}
	}
	chain := ExtensionChainSQL(q)
	for _, want := range []string{"WITH RECURSIVE", "pg_inherits", "pg_depend", "deptype = 'e'", "pg_namespace", "pg_extension"} {
		if !strings.Contains(chain, want) {
			t.Errorf("extension chain missing %q", want)
		}
	}
}

func TestRepackVersions(t *testing.T) {
	v, err := NormalizeRepackVersions("pg_repack 1.5.3", "1.5.3", "pg_repack 1.5.3", "1.5.3")
	if err != nil || !v.Match() {
		t.Fatalf("matching versions: %v %v", v, err)
	}
	v, err = NormalizeRepackVersions("pg_repack 1.5.3", "1.5.2", "pg_repack 1.5.3", "1.5.3")
	if err != nil || v.Match() {
		t.Fatalf("mismatch accepted: %v %v", v, err)
	}
	if _, err := NormalizeRepackVersion("pg_repack 1.5.2 extra"); err == nil {
		t.Error("ambiguous version accepted")
	}
	if _, err := NormalizeRepackVersion(""); err == nil {
		t.Error("empty version accepted")
	}
	sql := RepackVersionsSQL("repack")
	for _, want := range []string{`"repack".version()`, `"repack".version_sql()`, "extension_version",
		"db_default_tablespace", "session_default_tablespace", "dattablespace"} {
		if !strings.Contains(sql, want) {
			t.Errorf("version SQL missing %q: %s", want, sql)
		}
	}
}

func TestRepackTablespaceFacts(t *testing.T) {
	q, _ := QuoteRelation("public.events")
	sql := RepackTablespaceSQL(q)
	for _, want := range []string{"reltablespace", "pg_database", "pg_tablespace", "reltoastrelid", "pg_index",
		"CASE WHEN r.reltablespace = 0 THEN d.dattablespace"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	facts := ParseTablespaceFacts([]map[string]string{
		{"kind": "table", "name": "events", "tablespace": "pg_default"},
		{"kind": "index", "name": "events_pkey", "tablespace": "pg_default"},
	})
	if len(facts) != 2 || !facts[0].SupportedPGDATA() || !facts[1].SupportedPGDATA() {
		t.Fatalf("%+v", facts)
	}
	if (TablespaceFact{Tablespace: "fastspace"}).SupportedPGDATA() {
		t.Error("external tablespace accepted")
	}
	for _, want := range []string{"pg_database", "dattablespace", "pg_tablespace"} {
		if !strings.Contains(RepackDBDefaultTablespaceSQL, want) {
			t.Errorf("database default SQL missing %q", want)
		}
	}
}

func TestRepackStorageEvidence(t *testing.T) {
	q, _ := QuoteRelation("public.events")
	sql := RepackStorageSQL(q)
	for _, want := range []string{"oid", "relfilenode", "reltablespace", "pg_database", "current_database", "to_regclass"} {
		if !strings.Contains(sql, want) {
			t.Errorf("storage SQL missing %q: %s", want, sql)
		}
	}
	before := ParseRepackStorage(map[string]string{"oid": "1", "relfilenode": "100", "tablespace": "pg_default"})
	after := ParseRepackStorage(map[string]string{"oid": "1", "relfilenode": "200", "tablespace": "pg_default"})
	if !RepackRewritten(before, after) {
		t.Error("changed relfilenode not accepted")
	}
	if RepackRewritten(before, before) || RepackRewritten(before, ParseRepackStorage(map[string]string{"oid": "2", "relfilenode": "200", "tablespace": "pg_default"})) {
		t.Error("unchanged OID/relfilenode or changed OID accepted")
	}
}

func TestRepackSpaceNeed(t *testing.T) {
	got, err := RepackSpaceNeed(100)
	if err != nil || got != 200 {
		t.Fatalf("space %d %v", got, err)
	}
	if _, err := RepackSpaceNeed(-1); err == nil {
		t.Error("negative size accepted")
	}
}

func TestExactBloatSQLUsesFullScan(t *testing.T) {
	sql, err := ExactBloatSQL("public.events", "public")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `"public".pgstattuple('"public"."events"'::regclass)`) {
		t.Errorf("not a schema-qualified full scan: %s", sql)
	}
	if strings.Contains(sql, "pgstattuple_approx") {
		t.Error("approximate function used")
	}
	for _, want := range []string{"table_len", "tuple_count", "tuple_len", "tuple_percent", "dead_tuple_count",
		"dead_tuple_len", "dead_tuple_percent", "free_space", "free_percent"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q", want)
		}
	}
	if _, err := ExactBloatSQL("bad;name", "public"); err == nil {
		t.Error("bad table accepted")
	}
}

func TestParseTableStats(t *testing.T) {
	row := map[string]string{
		"table_len": "1000", "tuple_count": "10", "tuple_len": "700", "tuple_percent": "70.0",
		"dead_tuple_count": "2", "dead_tuple_len": "100", "dead_tuple_percent": "10.0",
		"free_space": "200", "free_percent": "20.0",
	}
	got, err := ParseTableStats("app", "public", "events", []map[string]string{row})
	if err != nil {
		t.Fatal(err)
	}
	if got.TableBytes != 1000 || got.TupleCount != 10 || got.DeadTupleBytes != 100 || got.FreeBytes != 200 ||
		got.FreePercent != 20 || got.Method != "pgstattuple" {
		t.Fatalf("%+v", got)
	}
	if _, err := ParseTableStats("app", "public", "events", nil); err == nil {
		t.Error("no rows accepted")
	}
	bad := map[string]string{"table_len": "not-a-number"}
	if _, err := ParseTableStats("app", "public", "events", []map[string]string{bad}); err == nil {
		t.Error("bad number accepted")
	}
}

func TestPgstattupleSchemaSQL(t *testing.T) {
	for _, want := range []string{"pg_proc", "proname = 'pgstattuple'", "pg_extension", "pg_depend"} {
		if !strings.Contains(PgstattupleSchemaSQL, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRepackCounts(t *testing.T) {
	var c RepackCounts
	c.AddRepacked()
	c.AddFailed()
	c.AddSkipped(RepackReasonNoKey)
	c.AddBusy()
	if c.Selected != 4 || c.Repacked != 1 || c.Failed != 1 || c.Skipped != 1 || c.Busy != 1 ||
		c.SkippedByReason[RepackReasonNoKey] != 1 {
		t.Fatalf("%+v", c)
	}
}
