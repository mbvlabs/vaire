package cli

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/steps"
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

func TestBackupRowLinesIdleVsLagging(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	zero, three := int64(0), int64(3)
	data := backupRowsData{Databases: []backupRowData{
		{Database: "idle", Stanza: "idle-1", Status: "ok", Sets: 1,
			Window: windowData{From: "2026-10-14T09:00:01Z", To: "2026-10-14T10:00:00Z", LagSeconds: 7200, UpToDate: true, PendingWAL: &zero}},
		{Database: "busy", Stanza: "busy-1", Status: "ok", Sets: 2,
			Window: windowData{From: "2026-10-14T09:00:01Z", To: "2026-10-14T11:51:00Z", LagSeconds: 540, PendingWAL: &three}},
	}}
	lines := backupRowLines(data, now)
	if !strings.Contains(lines[1], "from 2026-10-14 09:00Z, up to date") || strings.Contains(lines[1], "lag") {
		t.Errorf("idle row: %q", lines[1])
	}
	if !strings.Contains(lines[2], "from 2026-10-14 09:00Z, lag 9m") {
		t.Errorf("lagging row: %q", lines[2])
	}
	idle := data.Databases[0].Window.describe()
	if idle != "2026-10-14T09:00:01Z to 2026-10-14T10:00:00Z (archive up to date)" {
		t.Errorf("idle describe = %q", idle)
	}
	busy := data.Databases[1].Window.describe()
	if busy != "2026-10-14T09:00:01Z to 2026-10-14T11:51:00Z (archive lag 9m, 3 WAL segments waiting)" {
		t.Errorf("lagging describe = %q", busy)
	}
	one := int64(1)
	w := windowData{From: "x", To: "y", LagSeconds: 30, PendingWAL: &one}
	if got := w.archiveState(); got != "archive lag 30s, 1 WAL segment waiting" {
		t.Errorf("one waiting = %q", got)
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{0: "sets", 1: "set", 2: "sets"} {
		if got := plural(n, "set", "sets"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestBackupRowLinesKeptStanzas(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	window := windowData{From: "2026-10-14T10:21:21Z", To: "2026-10-14T10:21:21Z"}
	data := backupRowsData{Databases: []backupRowData{
		{Database: "app2", Stanza: "app2-506c21", Status: "ok", Sets: 1,
			Window: windowData{From: "2026-10-14T09:00:01Z", To: "2026-10-14T10:00:00Z", UpToDate: true}},
		{Database: "app", Stanza: "app-c273db", Status: "ok", Sets: 1, Window: window,
			DroppedAt: "2026-10-14T10:23:22Z", ExpiresAt: "2026-10-21T10:23:22Z"},
		{Database: "app2", Stanza: "app2-111111", Status: "ok", Sets: 2, Window: window,
			DroppedAt: "2026-10-14T11:00:00Z", ExpiresAt: "2026-10-21T11:00:00Z", Reason: backup.DroppedReasonReplaced},
	}}
	lines := backupRowLines(data, now)
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "archive lag") || strings.Contains(text, "lag 0s") {
		t.Errorf("kept stanzas show a lag:\n%s", text)
	}
	var header, dropped, replaced string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "KEPT UNTIL"):
			header = l
		case strings.Contains(l, "app-c273db"):
			dropped = l
		case strings.Contains(l, "app2-111111"):
			replaced = l
		}
	}
	for _, col := range []string{"DATABASE", "STANZA", "SETS", "ENDED", "KEPT UNTIL", "WINDOW"} {
		if !strings.Contains(header, col) {
			t.Errorf("kept header lacks %s: %q", col, header)
		}
	}
	for _, want := range []string{"dropped 2026-10-14 10:23Z", "2026-10-21 10:23Z", "2026-10-14 10:21Z to 2026-10-14 10:21Z"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("dropped row lacks %q: %q", want, dropped)
		}
	}
	if !strings.Contains(replaced, "replaced by restore 2026-10-14 11:00Z") || strings.Contains(replaced, "dropped") {
		t.Errorf("replaced row: %q", replaced)
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

func TestAddRecoveryKit(t *testing.T) {
	values := map[string]string{"svc.eru_monitor": "pw"}
	addRecoveryKit(values, nil)
	if len(values) != 1 {
		t.Errorf("no kit: secrets = %v", values)
	}
	// server init creates the recovery identity without a bucket: no keys to send.
	identityOnly := &contexts.BackupCreds{}
	identityOnly.Dumps.AgeIdentity, identityOnly.Dumps.AgeRecipient = "AGE-SECRET-KEY-1", "age1xyz"
	addRecoveryKit(values, identityOnly)
	if len(values) != 1 {
		t.Errorf("identity only: secrets = %v", values)
	}
	kit := &contexts.BackupCreds{AccessKeyID: "AKID", SecretAccessKey: "s3cret", Bucket: "eru-backups-test", Prefix: "mbvlabs"}
	kit.PgBackRest.CipherPass = "cipher"
	kit.Dumps.AgeIdentity, kit.Dumps.AgeRecipient = "AGE-SECRET-KEY-1", "age1xyz"
	addRecoveryKit(values, kit)
	want := map[string]string{
		"svc.eru_monitor":              "pw",
		steps.SecretBackupKeyID:        "AKID",
		steps.SecretBackupKeySecret:    "s3cret",
		steps.SecretBackupCipherPass:   "cipher",
		steps.SecretBackupAgeRecipient: "age1xyz",
	}
	if !maps.Equal(values, want) {
		t.Errorf("bucket kit: secrets = %v, want %v", values, want)
	}
	if _, ok := values[steps.SecretBackupAgeIdentity]; ok {
		t.Error("the age identity never leaves the operator outside a dump restore")
	}
}

func TestInitBackupPhaseText(t *testing.T) {
	started, completed := initBackupPhaseText("eru-backups-test", 0)
	for _, line := range []string{started, completed} {
		if strings.Contains(line, "first full backup taken") || strings.Contains(line, "take the first full") {
			t.Errorf("no databases: %q promises a backup init doesn't take", line)
		}
	}
	if !strings.Contains(started, "eru-backups-test") {
		t.Errorf("started = %q, want the bucket", started)
	}
	if _, completed := initBackupPhaseText("b", 1); completed != "WAL archiving on, first full backup taken" {
		t.Errorf("one database: completed = %q", completed)
	}
}
