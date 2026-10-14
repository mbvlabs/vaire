package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
)

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for ts, want := range map[string]string{
		"2026-10-14T11:59:30Z": "just now",
		"2026-10-14T11:15:00Z": "45m ago",
		"2026-10-14T10:00:00Z": "2h ago",
		"2026-10-11T12:00:00Z": "3d ago",
		"not a time":           "not a time",
	} {
		if got := ago(ts, now); got != want {
			t.Errorf("ago(%s) = %q, want %q", ts, got, want)
		}
	}
}

func TestBackupRowLines(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	data := backupRowsData{Databases: []backupRowData{{
		Database: "app", Stanza: "app-82446f", Status: "ok", Sets: 3,
		Window: windowData{From: "2026-10-07T02:00:01Z", To: "2026-10-14T11:59:00Z", LagSeconds: 60},
		Latest: &backup.Set{Type: "incr", Stop: "2026-10-14T10:00:00Z"},
		Verify: &backup.VerifyResult{OK: true, FinishedAt: "2026-10-14T10:00:00Z"},
		Drill:  &backup.Drill{Result: backup.DrillFail, FinishedAt: "2026-10-11T12:00:00Z"},
		Dump:   &backup.Dump{Created: "2026-10-13T12:00:00Z"},
	}}}
	lines := backupRowLines(data, now)
	for _, col := range []string{"STATUS", "DUMP", "VERIFY", "DRILL"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header lacks %s: %q", col, lines[0])
		}
	}
	for _, want := range []string{"ok", "incr 2h ago", "passed 2h ago", "FAILED 3d ago", "24h ago", "from 2026-10-07 02:00Z, lag 1m"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row lacks %q: %q", want, lines[1])
		}
	}
	for _, l := range lines {
		if len(l) > 120 {
			t.Errorf("line is %d columns: %q", len(l), l)
		}
	}
}

func TestNotStarted(t *testing.T) {
	dbs := []string{"app", "other", "third"}
	if got := notStarted(dbs, nil); !slices.Equal(got, []string{"other", "third"}) {
		t.Errorf("detached on the first: %v", got)
	}
	if got := notStarted(dbs, make([]dbJobOutcome, 2)); len(got) != 0 {
		t.Errorf("detached on the last: %v", got)
	}
}
