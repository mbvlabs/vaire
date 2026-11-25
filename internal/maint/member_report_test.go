package maint

import (
	"strings"
	"testing"
)

func TestMemberReportSQLScopesSharedCatalogs(t *testing.T) {
	for kind, filters := range map[string][]string{
		ReportWraparound:  {"datname = current_database()"},
		ReportLongQueries: {"datname = current_database()"},
		ReportLocks:       {"a.datname = current_database()", "b.datname = current_database()"},
	} {
		sql, err := MemberReportSQL(kind, 300)
		if err != nil {
			t.Fatal(err)
		}

		for _, filter := range filters {
			if !strings.Contains(sql, filter) {
				t.Fatalf("%s missing %s: %s", kind, filter, sql)
			}
		}

		admin, _ := ReportSQL(kind, 300)
		if strings.Contains(admin, filters[0]) {
			t.Fatalf("admin %s was scoped: %s", kind, admin)
		}
	}
}
