package node

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExactReportScansAdmittedOIDAndRechecksCanonicalName(t *testing.T) {
	scanned, lookups := false, 0
	local := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
		switch {
		case strings.Contains(sql, "relkind"):
			lookups++
			if lookups == 2 && !strings.Contains(sql, `'"tenant"."events"'`) {
				t.Fatalf("identity recheck used the caller's search_path: %s", sql)
			}
			return []map[string]string{{
				"oid": "4242", "schema": "tenant", "table": "events",
				"kind": "r", "persistence": "p", "access_method": "heap",
			}}, nil
		case strings.Contains(sql, "table_len"):
			scanned = true
			if !strings.Contains(sql, `.pgstattuple(4242::oid::regclass)`) {
				t.Fatalf("scan did not pin the admitted OID: %s", sql)
			}
		}
		return repackDefaultAnswer(db, sql)
	}}
	installRepackSeams(t, local)
	cfg := repackTestConfig(t)
	stats, err := exactTableStats(context.Background(), cfg.Clusters[0], "events")
	if err != nil {
		t.Fatal(err)
	}
	if !scanned || lookups != 2 || stats.Schema != "tenant" || stats.Table != "events" {
		t.Fatalf("scan=%v lookups=%d stats=%+v", scanned, lookups, stats)
	}
	if local.execs != 0 {
		t.Fatal("read-only measurement executed a mutation")
	}
}

func TestExactReportDoesNotAttributeChangedOrUnprovenIdentity(t *testing.T) {
	for _, state := range []string{"replaced", "renamed", "missing", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			scanned := false
			local := &fakeRepackPG{answer: func(db, sql string) ([]map[string]string, error) {
				if strings.Contains(sql, "table_len") {
					scanned = true
				}
				if !strings.Contains(sql, "relkind") {
					return repackDefaultAnswer(db, sql)
				}
				facts := map[string]string{
					"oid": "42", "schema": "public", "table": "events",
					"kind": "r", "persistence": "p", "access_method": "heap",
				}
				if scanned {
					switch state {
					case "replaced":
						facts["oid"] = "43"
					case "renamed":
						facts["table"] = "renamed"
					case "missing":
						return nil, nil
					case "unreadable":
						return nil, errors.New("catalog unavailable")
					}
				}
				return []map[string]string{facts}, nil
			}}
			installRepackSeams(t, local)
			cfg := repackTestConfig(t)
			stats, err := exactTableStats(context.Background(), cfg.Clusters[0], "events")
			if err == nil || reason(t, err) != "exact_bloat_failed" || stats.Method != "" {
				t.Fatalf("attributed an unproven measurement: stats=%+v err=%v", stats, err)
			}
			if !scanned {
				t.Fatal("test did not cross the measurement boundary")
			}
		})
	}
}
