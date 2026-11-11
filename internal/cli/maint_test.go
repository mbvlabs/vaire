package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/maint"
)

func TestValidateMaintReportFlags(t *testing.T) {
	if err := validateMaintReportFlags(maint.ReportBloat, true, "app", "public.events"); err != nil {
		t.Fatal(err)
	}
	if err := validateMaintReportFlags(maint.ReportBloat, false, "app", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind  string
		exact bool
		db    string
		table string
	}{
		{maint.ReportWraparound, true, "app", "events"},
		{maint.ReportBloat, true, "", "events"},
		{maint.ReportBloat, true, "app", ""},
		{maint.ReportBloat, false, "app", "events"},
	} {
		if err := validateMaintReportFlags(tc.kind, tc.exact, tc.db, tc.table); err == nil {
			t.Errorf("%+v passed", tc)
		}
	}
}

func TestRepackResultMessageUnknown(t *testing.T) {
	run := maint.RepackRun{
		DB:      "app",
		Unknown: map[string]string{"public.events": "completion_unknown/clean_exit: pg_repack exited successfully, but no completed rewrite could be attributed"},
		Counts:  maint.RepackCounts{Selected: 1, Unknown: 1},
	}
	message, warnings := repackResultMessage("app", run)
	for _, want := range []string{"command exited successfully", "rewrite completion could not be confirmed", "public.events", "completion_unknown"} {
		if !strings.Contains(message, want) {
			t.Errorf("unknown message missing %q in\n%s", want, message)
		}
	}
	if strings.Contains(message, "repacked in app") || strings.Contains(message, "failed:") {
		t.Errorf("unknown claimed as repacked or failed: %s", message)
	}
	if len(warnings) != 1 || warnings[0].Code != maint.RepackReasonCompletionUnknown {
		t.Fatalf("unknown warning: %+v", warnings)
	}
	for _, want := range []string{"public.events", "unchanged or unknown"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("unknown warning missing %q: %s", want, warnings[0].Message)
		}
	}
	run.Unknown["public.events"] = "completion_unknown/process_error: exit status 1; storage changed, but the rewrite completion is not attributable"
	message, _ = repackResultMessage("app", run)
	if !strings.Contains(message, "the pg_repack process failed") || !strings.Contains(message, "rewrite completion could not be confirmed") {
		t.Errorf("process error unknown message: %s", message)
	}
	if strings.Contains(message, "command exited successfully") {
		t.Errorf("process error claimed clean exit: %s", message)
	}
	run.Unknown["public.events"] = "completion_unknown/not_started: storage identity unavailable before execution: no readable relation"
	message, _ = repackResultMessage("app", run)
	if !strings.Contains(message, "the pg_repack command was not started") || strings.Contains(message, "command exited successfully") {
		t.Errorf("not-started unknown message: %s", message)
	}
	run.Unknown["public.events"] = "completion_unknown/clean_exit: process error is only part of the untrusted table name"
	message, _ = repackResultMessage("app", run)
	if !strings.Contains(message, "command exited successfully") || strings.Contains(message, "the pg_repack process failed") {
		t.Errorf("untrusted detail spoofed the outcome: %s", message)
	}
}

func TestRepackResultMessageConfirmedAndEmpty(t *testing.T) {
	message, warnings := repackResultMessage("app", maint.RepackRun{DB: "app", Repacked: []string{"public.events"}})
	if !strings.Contains(message, "repacked in app: public.events") || len(warnings) != 0 {
		t.Fatalf("confirmed message %q warnings %+v", message, warnings)
	}
	message, warnings = repackResultMessage("app", maint.RepackRun{DB: "app"})
	if !strings.Contains(message, "nothing was repacked") || len(warnings) != 0 {
		t.Fatalf("empty message %q warnings %+v", message, warnings)
	}
	message, warnings = repackResultMessage("app", maint.RepackRun{DB: "app",
		Skipped: []maint.RepackSkip{{Table: "public.events", Reason: maint.RepackReasonLockTimeout}}})
	if !strings.Contains(message, "nothing in app was repacked") || !strings.Contains(message, "public.events (repack_lock_timeout)") || len(warnings) != 0 {
		t.Fatalf("skipped message %q warnings %+v", message, warnings)
	}
}

func TestRepackVersionDisclosureIsDeferred(t *testing.T) {
	message := repackVersionDisclosure("deferred_to_job")
	for _, want := range []string{"deferred to the job", "under the job's locks", "checked"} {
		if !strings.Contains(message, want) {
			t.Errorf("deferred disclosure missing %q: %s", want, message)
		}
	}
	for _, unwanted := range []string{"versions matched", "version match confirmed", "matched"} {
		if strings.Contains(message, unwanted) {
			t.Errorf("deferred disclosure claims %q: %s", unwanted, message)
		}
	}
	if repackVersionDisclosure("") != "" {
		t.Error("disclosure added without deferral")
	}
}

func TestRepackPreflight(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Protected: true}, {Name: "follower", Follows: "app"}}}
	if err := repackPreflight(cfg, "app"); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected: %v", err)
	}
	if err := repackPreflight(cfg, "follower"); err == nil || !strings.Contains(err.Error(), "follower") {
		t.Fatalf("follower: %v", err)
	}
	if err := repackPreflight(cfg, "other"); err != nil {
		t.Fatalf("unknown database preflight: %v", err)
	}
}

func TestReportMessageExact(t *testing.T) {
	msg := reportMessage(maint.ReportBloat, []maintReportDB{{DB: "app", Exact: &maint.TableStats{
		Schema: "public", Table: "events", Method: "pgstattuple", TableBytes: 1000, TupleCount: 10,
		TupleBytes: 700, TuplePercent: 70, DeadTupleCount: 2, DeadTupleBytes: 100, DeadTuplePercent: 10,
		FreeBytes: 200, FreePercent: 20,
	}}})
	for _, want := range []string{"public.events", "pgstattuple", "full table scan", "read lock", "not guaranteed reclaimable", "not its indexes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "exact bloat") || strings.Contains(msg, "estimated bloat") {
		t.Errorf("exact message claims bloat: %s", msg)
	}
}
