package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/doctor"
	"github.com/mbvlabs/eru/internal/maint"
)

func TestStatsTokenCLI(t *testing.T) {
	if statsToken("prod", "") != "prod/stats" || statsToken("prod", "app") != "app/stats" {
		t.Error("stats token")
	}
}

func TestStatusMessage(t *testing.T) {
	tps, hit := 12.5, 0.991
	var cur int64 = 300
	d := statusData{Server: "prod", Node: "db1", EruVersion: "1.0.0", PGVersion: "18.1",
		Poolers:   map[string]statusPools{"transaction": {ClientsActive: 2}},
		Databases: []statusDB{{Name: "app", State: "running", Size: "small", MemoryMiB: 512, MemoryCurrentMiB: &cur, TPS: &tps, CacheHitRatio: &hit, RestartPending: true}},
	}
	d.Budget.RAMMiB, d.Budget.FreeMiB = 4096, -100
	out := statusMessage(d, nil)
	for _, want := range []string{"server prod", "PostgreSQL 18.1", "overcommitted by", "app", "running*", "12.5", "99.1%", "restart pending", "backups none off the server"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if !strings.Contains(statusMessage(statusData{Backups: true}, nil), "no databases yet") {
		t.Error("empty server")
	}
	if statusNext(d) == nil {
		t.Error("a pending restart suggests doctor")
	}
}

func TestDoctorMessage(t *testing.T) {
	out := doctorMessage([]doctor.Result{
		{ID: "pg.ssl", DB: "app", Status: doctor.Pass},
		{ID: "host.disk_data", Status: doctor.Warn, Detail: "15% free", Fix: "free space"},
		{ID: "tls.expiry", Status: doctor.Fail, Detail: "5 days"},
	})
	if !strings.HasPrefix(out, "FAIL tls.expiry") || !strings.Contains(out, "fix: free space") || !strings.Contains(out, "1 pass · 1 warn · 1 fail · 0 skip") {
		t.Errorf("%s", out)
	}
	if healthLine(map[doctor.Status]int{doctor.Pass: 3}) != "health: 3 pass, 0 warn, 0 fail" {
		t.Error("health line")
	}
}

func TestReportMessage(t *testing.T) {
	out := reportMessage(maint.ReportBloat, []maintReportDB{{DB: "app", Bloat: &maint.BloatReport{
		Rows:        []maint.BloatRow{{Schema: "public", Table: "events", BloatBytes: 1 << 20, RealSize: 2 << 20, Ratio: 0.5}},
		Unestimated: []string{"public.x"},
	}}, {DB: "other", Error: "the cluster is not running"}})
	for _, want := range []string{"public.events", "50.0%", "not estimated", "the cluster is not running", "estimate from statistics"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	out = reportMessage(maint.ReportWraparound, []maintReportDB{{DB: "app", Rows: []map[string]string{{"kind": "database", "name": "app", "xid_age": "5", "status": "pass"}}}})
	if !strings.Contains(out, "database  app  5") {
		t.Errorf("%s", out)
	}
}

func TestSnippets(t *testing.T) {
	p := prometheusSnippet("prod", "https://db1.example.com/metrics/targets.json", "metrics", "pw")
	if !strings.Contains(p, "http_sd_configs") || !strings.Contains(p, "job_name: eru-prod") {
		t.Errorf("%s", p)
	}
	a := alloySnippet("my-prod", "https://x/metrics/targets.json", "metrics", "pw")
	if !strings.Contains(a, `discovery.http "eru_my_prod"`) {
		t.Errorf("%s", a)
	}
}
