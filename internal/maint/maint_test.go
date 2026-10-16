package maint

import (
	"strings"
	"testing"
)

func TestEmbeddedSQL(t *testing.T) {
	for _, name := range []string{"table_bloat.sql", "btree_bloat.sql"} {
		s := sqlFile(name)
		if !strings.Contains(s, "pgsql-bloat-estimation") || strings.HasSuffix(s, ";") || !strings.Contains(s, "SELECT") {
			t.Errorf("%s: header, trailing semicolon or body", name)
		}
	}
	b := BloatSQL()
	if strings.Count(b, "FROM (") < 2 || !strings.Contains(b, "UNION ALL") {
		t.Errorf("bloat sql:\n%s", b)
	}
	if !strings.Contains(License(), "Redistribution") {
		t.Error("the licence is not embedded")
	}
	for _, kind := range ReportKinds {
		if _, err := ReportSQL(kind, 300); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if q, _ := ReportSQL(ReportLongQueries, 600); !strings.Contains(q, "make_interval(secs => 600)") {
		t.Error("long-queries threshold")
	}
	if _, err := ReportSQL("nope", 0); err == nil || ValidReport("nope") {
		t.Error("an unknown report")
	}
}

func TestParseBloat(t *testing.T) {
	r := ParseBloat("app", []map[string]string{
		{"kind": "table", "schemaname": "public", "table_name": "events", "bloat_bytes": "5000000", "real_size": "10000000", "bloat_pct": "50.0", "fillfactor": "100", "is_na": "f"},
		{"kind": "index", "schemaname": "public", "table_name": "events", "index_name": "events_pkey", "bloat_bytes": "9000000", "real_size": "12000000", "bloat_pct": "75", "fillfactor": "90", "is_na": "f"},
		{"kind": "table", "schemaname": "public", "table_name": "small", "bloat_bytes": "100", "is_na": "f"},
		{"kind": "table", "schemaname": "public", "table_name": "nostats", "bloat_bytes": "0", "is_na": "t"},
	}, 1000)
	if len(r.Rows) != 2 || r.Rows[0].Index != "events_pkey" || r.Rows[0].Ratio != 0.75 || r.Rows[1].BloatBytes != 5000000 {
		t.Errorf("%+v", r.Rows)
	}
	if len(r.Unestimated) != 1 || r.Unestimated[0] != "public.nostats" {
		t.Errorf("unestimated %v", r.Unestimated)
	}
	if tot := TotalBloat(r); tot["index"] != 9000000 || tot["table"] != 5000000 {
		t.Errorf("total %v", tot)
	}
}

func TestStatements(t *testing.T) {
	cases := []struct {
		o    Options
		want string
	}{
		{Options{Kind: Vacuum, DB: "app", Table: "events", Analyze: true, Parallel: 2}, `VACUUM (ANALYZE true, FREEZE false, PARALLEL 2) "events"`},
		{Options{Kind: Vacuum, DB: "app", Table: "sales.Orders", Freeze: true}, `VACUUM (ANALYZE false, FREEZE true) "sales"."Orders"`},
		{Options{Kind: Analyze, DB: "app", Table: "events"}, `ANALYZE "events"`},
		{Options{Kind: Reindex, DB: "app", Index: "events_pkey"}, `REINDEX INDEX CONCURRENTLY "events_pkey"`},
		{Options{Kind: Reindex, DB: "app", Table: "events"}, `REINDEX TABLE CONCURRENTLY "events"`},
		{Options{Kind: Reindex, DB: "app"}, `REINDEX DATABASE CONCURRENTLY "app"`},
	}
	for _, c := range cases {
		got, err := c.o.Statements()
		if err != nil || len(got) != 1 || got[0] != c.want {
			t.Errorf("%+v: %v %v", c.o, got, err)
		}
	}
	if got, _ := (Options{Kind: Vacuum, DB: "app"}).Statements(); got != nil {
		t.Error("a database-wide vacuum runs vacuumdb")
	}
	for _, bad := range []Options{
		{Kind: Reindex, DB: "app", NotConcurrently: true},
		{Kind: Vacuum, DB: "app", Table: "x; drop table y"},
		{Kind: Vacuum, DB: "app", Index: "i"},
		{Kind: "cluster", DB: "app"},
		{Kind: Vacuum},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v passed", bad)
		}
	}
}

func TestHeavyAndVacuumdb(t *testing.T) {
	if !(Options{Kind: Vacuum}).Heavy() || (Options{Kind: Vacuum, Table: "t"}).Heavy() || !(Options{Kind: Reindex, Table: "t"}).Heavy() || (Options{Kind: Analyze}).Heavy() {
		t.Error("heavy")
	}
	got := strings.Join(Options{Kind: Vacuum, DB: "app", Analyze: true, Freeze: true}.VacuumdbArgs("/var/run/postgresql", "eru_admin", 5433, 2), " ")
	if got != "-h /var/run/postgresql -p 5433 -U eru_admin --jobs=2 --analyze --freeze --dbname=app" {
		t.Errorf("vacuumdb %s", got)
	}
	got = strings.Join(Options{Kind: Analyze, DB: "app"}.VacuumdbArgs("/s", "postgres", 5433, 0), " ")
	if !strings.Contains(got, "--analyze-only") || !strings.Contains(got, "--jobs=1") || !strings.Contains(got, "-U postgres") {
		t.Errorf("analyze %s", got)
	}
}

func TestSelectBackends(t *testing.T) {
	all := ParseBackends([]map[string]string{
		{"pid": "1", "role": "app_app", "state": "idle in transaction", "state_seconds": "900", "superuser": "f"},
		{"pid": "2", "role": "app_app", "state": "idle in transaction", "state_seconds": "10", "superuser": "f"},
		{"pid": "3", "role": "eru_admin", "state": "idle in transaction", "state_seconds": "900", "superuser": "f"},
		{"pid": "4", "role": "postgres", "state": "active", "superuser": "t"},
		{"pid": "5", "role": "app_ro", "state": "idle in transaction (aborted)", "state_seconds": "900", "app": "eru top"},
		{"pid": "6", "role": "app_app", "state": "active", "state_seconds": "900"},
	})
	sel, _ := SelectBackends(all, KillOptions{IdleInTx: true, OlderThanSecond: 600})
	if len(sel) != 1 || sel[0].PID != 1 {
		t.Errorf("idle-in-tx %+v", sel)
	}
	sel, refused := SelectBackends(all, KillOptions{PID: 4})
	if len(sel) != 0 || len(refused) != 1 {
		t.Error("a superuser's pid must be refused")
	}
	sel, _ = SelectBackends(all, KillOptions{PID: 3})
	if len(sel) != 1 {
		t.Error("an explicit pid of an eru role is allowed")
	}
	if TerminateSQL([]int{1, 2}) != "SELECT pid, pg_terminate_backend(pid) AS terminated FROM unnest(ARRAY[1,2]::int[]) AS pid" {
		t.Error("terminate sql")
	}
}

func TestQuoteRelationAndWraparound(t *testing.T) {
	if q, err := QuoteRelation("public.events"); err != nil || q != `"public"."events"` {
		t.Errorf("%s %v", q, err)
	}
	if _, err := QuoteRelation(`a"b`); err == nil {
		t.Error("a quote passed")
	}
	if WraparoundStatus(5) != "pass" || WraparoundStatus(1_200_000_000) != "warn" || WraparoundStatus(2_000_000_000) != "fail" {
		t.Error("wraparound")
	}
}
