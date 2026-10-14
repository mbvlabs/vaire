package node

import (
	"os"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
)

func TestJournalNoise(t *testing.T) {
	for line, want := range map[string]bool{
		"pam_unix(runuser:session): session opened for user postgres(uid=106) by (uid=0)":                               true,
		"pam_unix(runuser:session): session closed for user postgres":                                                   true,
		"backup 20261014-101010F finished":                                                                              false,
		"pam_unix(sshd:session): session opened for user root":                                                          false,
		"Synchronizing state of pgbouncer.service with SysV service script with /usr/lib/systemd/systemd-sysv-install.": true,
		"Executing: /usr/lib/systemd/systemd-sysv-install enable pgbouncer":                                             true,
		"pgbouncer started": false,
	} {
		if got := journalNoise(line); got != want {
			t.Errorf("journalNoise(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestJournalFilterDropsEnvelope(t *testing.T) {
	lines := []string{
		`{"schema":"eru-event/v1","type":"step","step":"backup","ts":"2026-10-14T12:00:00Z"}`,
		"backup 20261014-101010F finished",
		"Executing: /usr/lib/systemd/systemd-sysv-install enable pgbouncer",
		`{"schema":"eru-event/v1","type":"result"}`,
		"{",
		`  "schema": "eru/v1",`,
		`  "ok": true,`,
		`  "data": {`,
		`    "label": "20261014-101010F"`,
		`  }`,
		"}",
		"after",
	}
	var f journalFilter
	var got []output.Event
	for _, l := range lines {
		if ev, ok := f.event(l); ok {
			got = append(got, ev)
		}
	}
	if len(got) != 3 || got[0].Type != "step" || got[0].TS != "" || got[1].Line != "backup 20261014-101010F finished" || got[2].Line != "after" {
		t.Fatalf("events = %+v", got)
	}
}

func TestJobErrorDetailsFlattens(t *testing.T) {
	rec := engine.JobRecord{ID: "01M4X0ME0NBQ95HNNDT213BGH9", Job: "backup-full"}
	got := jobErrorDetails(rec, map[string]any{"output_tail": []any{"ERROR: x"}, "step": "backup"})
	if got["job_id"] != rec.ID || got["job"] != rec.Job || got["step"] != "backup" {
		t.Fatalf("details = %v", got)
	}
	if tail, ok := got["output_tail"].([]any); !ok || len(tail) != 1 {
		t.Fatalf("output_tail = %v", got["output_tail"])
	}
	if got := jobErrorDetails(rec, "text"); got["error_details"] != "text" {
		t.Fatalf("non-object details = %v", got)
	}
	if got := jobErrorDetails(rec, nil); len(got) != 2 {
		t.Fatalf("nil details = %v", got)
	}
}

func TestTimerUnit(t *testing.T) {
	cases := map[string]engine.JobRecord{
		"eru-db-dump@app2.service":  {Job: "db-dump", DB: "app2"},
		"eru-stanza-expiry.service": {Job: "stanza-expiry"},
		"":                          {Job: "restore", DB: "app"},
	}
	for want, rec := range cases {
		if got := timerUnit(rec); got != want {
			t.Errorf("timerUnit(%+v) = %q, want %q", rec, got, want)
		}
	}
}

func TestJobProcessAlive(t *testing.T) {
	// The test binary is alive but isn't eru node run backup-full: a reused
	// PID doesn't keep a dead job running.
	if _, err := os.Stat("/proc/self/cmdline"); err == nil && jobProcessAlive(os.Getpid(), "backup-full") {
		t.Fatal("the test process counted as eru node run backup-full")
	}
	if jobProcessAlive(1<<22+12345, "backup-full") {
		t.Fatal("a PID that doesn't exist counted as alive")
	}
}

func TestJobGoneByPID(t *testing.T) {
	rec := engine.JobRecord{ID: "01M4WZQ5RT0RCBPE36DAJ2E9MJ", Job: "restore-drill", State: engine.JobRunning, PID: 1<<22 + 12345}
	if !jobGone(t.Context(), rec) {
		t.Fatal("a running job whose process is gone isn't reported gone")
	}
	if jobGone(t.Context(), engine.JobRecord{Job: "restore-drill", State: engine.JobRunning}) {
		t.Fatal("a record with nothing to check was reported gone")
	}
}
