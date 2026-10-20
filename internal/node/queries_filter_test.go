package node

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/top"
	"github.com/mbvlabs/vaire/internal/pg"
)

// Even with --limit 1, an internal statement ranking ahead of application
// work must be excluded in SQL, not after the top-N rows have been fetched.
func TestStatementsFilterBeforeLimit(t *testing.T) {
	for _, query := range []struct {
		name string
		sql  func(bool) string
	}{
		{"queries", func(all bool) string { return statementsSQL("total", 1, all) }},
		{"top", top.StatementsSQL},
	} {
		t.Run(query.name, func(t *testing.T) {
			sql := query.sql(false)
			predicate := "AND NOT " + pg.VaireRoleSQL("r.rolname")
			filterAt := strings.Index(sql, predicate)
			orderAt := strings.Index(sql, "ORDER BY")
			limitAt := strings.Index(sql, "LIMIT")
			if filterAt < 0 || orderAt < filterAt || limitAt < orderAt || !strings.Contains(sql, "r.oid = s.userid") {
				t.Fatalf("filter must run before ranking/limit: %s", sql)
			}
			for _, role := range []string{"postgres", "pg_maintain", "pg_signal_backend"} {
				if !strings.Contains(sql[filterAt:orderAt], "'"+role+"'") {
					t.Errorf("administrative role %s not filtered before ranking/limit: %s", role, sql)
				}
			}
			if strings.Contains(query.sql(true), predicate) {
				t.Fatal("--all-sessions still filters internal roles")
			}
		})
	}
}

func TestParseStatementsAdministrativeRoles(t *testing.T) {
	rows := []map[string]string{
		{"role": "postgres", "query": "CREATE EXTENSION", "total_ms": "9.6"},
		{"role": "pg_maintain", "query": "REINDEX TABLE CONCURRENTLY", "total_ms": "4.0"},
		{"role": "pg_signal_backend", "query": "SELECT pg_terminate_backend", "total_ms": "3.0"},
		{"role": "app_owner", "query": "CREATE TABLE", "total_ms": "2.2"},
	}
	filtered := parseStatements("app", rows, false)
	got := mergeStatements(map[string][]statement{"app": filtered}, "total", 1)
	if len(got) != 1 || got[0].Role != "app_owner" {
		t.Fatalf("application work lost behind administrative statements: %+v", got)
	}
	all := parseStatements("app", rows, true)
	if len(all) != len(rows) {
		t.Fatalf("--all-sessions lost administrative statements: %+v", all)
	}
	got = mergeStatements(map[string][]statement{"app": all}, "total", 1)
	if len(got) != 1 || got[0].Role != "postgres" {
		t.Fatalf("--all-sessions must retain higher-ranked administrative work: %+v", got)
	}
}
