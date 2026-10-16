package node

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/top"
	"github.com/mbvlabs/eru/internal/pg"
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
			predicate := "AND NOT " + pg.EruRoleSQL("r.rolname")
			filterAt := strings.Index(sql, predicate)
			orderAt := strings.Index(sql, "ORDER BY")
			limitAt := strings.Index(sql, "LIMIT")
			if filterAt < 0 || orderAt < filterAt || limitAt < orderAt || !strings.Contains(sql, "r.oid = s.userid") {
				t.Fatalf("filter must run before ranking/limit: %s", sql)
			}
			if strings.Contains(query.sql(true), predicate) {
				t.Fatal("--all-sessions still filters internal roles")
			}
		})
	}
}
