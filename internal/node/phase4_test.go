package node

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/doctor"
	"github.com/mbvlabs/eru/internal/promfile"
)

func TestMergeStatements(t *testing.T) {
	per := map[string][]statement{
		"app":   {{DB: "app", QueryID: "1", TotalMS: 50, MeanMS: 5, Calls: 10}, {DB: "app", QueryID: "2", TotalMS: 10, MeanMS: 10, Calls: 1}},
		"other": {{DB: "other", QueryID: "3", TotalMS: 30, MeanMS: 1, Calls: 30}},
	}
	got := mergeStatements(per, "total", 2)
	if len(got) != 2 || got[0].QueryID != "1" || got[1].QueryID != "3" {
		t.Errorf("total %+v", got)
	}
	if got := mergeStatements(per, "mean", 1); got[0].QueryID != "2" {
		t.Errorf("mean %+v", got)
	}
	if got := mergeStatements(per, "calls", 1); got[0].QueryID != "3" {
		t.Errorf("calls %+v", got)
	}
	if !strings.Contains(statementsSQL("mean", 5), "ORDER BY s.mean_exec_time DESC LIMIT 5") {
		t.Error("order column")
	}
}

func TestTokens(t *testing.T) {
	cfg := config.Cluster{ClusterName: "prod"}
	if statsToken(cfg, json.RawMessage(`{"db":"app"}`)) != "app/stats" || statsToken(cfg, nil) != "prod/stats" {
		t.Error("stats token")
	}
	if killToken(cfg, json.RawMessage(`{"db":"app","pid":1}`)) != "app/kill" || killToken(cfg, nil) != "" {
		t.Error("kill token")
	}
	if metricsToken(cfg, nil) != "prod/metrics" {
		t.Error("metrics token")
	}
}

func TestParseSettingNames(t *testing.T) {
	got := parseSettingNames(strings.NewReader("# header\n\nwork_mem = '64MB'\n  shared_preload_libraries='x' # c\nbadline\n"))
	if strings.Join(got, ",") != "work_mem,shared_preload_libraries,badline" {
		t.Errorf("%v", got)
	}
}

func TestDoctorFamilies(t *testing.T) {
	results := []doctor.Result{
		{ID: "pg.ssl", DB: "app", Status: doctor.Pass},
		{ID: "host.disk_data", Status: doctor.Fail},
	}
	out := string(promfile.Render(doctorFamilies(time.Unix(100, 0), results)))
	for _, want := range []string{
		`eru_doctor_check{check="host.disk_data",db="",status="fail"} 1`,
		`eru_doctor_check{check="pg.ssl",db="app",status="pass"} 1`,
		`eru_doctor_last_run_timestamp_seconds 100`,
		`eru_doctor_failing_checks 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestMaintFamilies(t *testing.T) {
	st := maintState{
		LastDaily:  "2026-10-14T02:00:00Z",
		Wraparound: map[string]map[string]int64{"app": {"xid": 5, "mxid": 2}},
		Bloat:      map[string]map[string]int64{"app": {"table": 100, "index": 50}},
		Analyzed:   map[string]int{"app": 3},
	}
	out := string(promfile.Render(maintFamilies(st)))
	for _, want := range []string{
		`eru_wraparound_age{db="app"} 5`, `eru_mxid_age{db="app"} 2`,
		`eru_bloat_estimated_bytes{db="app",kind="index"} 50`, `eru_maint_analyzed_tables{db="app"} 3`,
		`eru_maint_last_run_timestamp_seconds{job="maint-daily"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, `job="maint-weekly"`) {
		t.Error("no weekly run yet, but a line")
	}
}

func TestWindowLeft(t *testing.T) {
	cfg := config.Cluster{Maintenance: config.Maintenance{Window: "02:00-05:00"}}
	in, left := windowLeft(cfg, time.Date(2026, 10, 14, 4, 30, 0, 0, time.Local))
	if !in || left != 30*time.Minute {
		t.Errorf("%v %v", in, left)
	}
	in, left = windowLeft(cfg, time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local))
	if in || left != 3*time.Hour {
		t.Errorf("outside the window: %v %v", in, left)
	}
}

func TestOpenAllowlists(t *testing.T) {
	cfg := config.Cluster{
		Network: config.Network{ClientAllowlist: []string{"0.0.0.0/0", "10.0.0.0/8"}, ExposePgbouncer: true},
		Metrics: config.Metrics{Allowlist: []string{"::/0"}},
	}
	got := openAllowlists(cfg)
	if len(got) != 2 {
		t.Errorf("%v", got)
	}
	cfg.Network.ExposePgbouncer = false
	if got := openAllowlists(cfg); len(got) != 1 {
		t.Errorf("unexposed poolers: %v", got)
	}
}

func TestStatusWarnings(t *testing.T) {
	w := statusWarnings(statusData{Databases: []statusDatabase{
		{Name: "app", State: "stopped"},
		{Name: "other", State: "running", RestartPending: true, DeferredRestart: "x", RestoreInProgress: true},
	}})
	codes := []string{}
	for _, x := range w {
		codes = append(codes, x.Code)
	}
	if strings.Join(codes, ",") != "database_stopped,restart_pending,restore_in_progress" {
		t.Errorf("%v", codes)
	}
	if !strings.Contains(w[1].Message, "maintenance window") {
		t.Error("a deferred restart names the window")
	}
}
