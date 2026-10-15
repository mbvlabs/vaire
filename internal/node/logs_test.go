package node

import (
	"strings"
	"testing"
)

// journalctl reads a bare "30m" as a timestamp and fails ("Failed to parse
// timestamp: 30m"); windows go to it in its relative form.
func TestJournalSince(t *testing.T) {
	for in, want := range map[string]string{
		"30m":              "-1800s",
		"1h":               "-3600s",
		"2h30m":            "-9000s",
		"2d":               "-172800s",
		"90s":              "-90s",
		"2026-10-15 06:40": "2026-10-15 06:40",
		"yesterday":        "yesterday",
		"-30m":             "-30m",
	} {
		if got := journalSince(in); got != want {
			t.Errorf("journalSince(%q) = %q, want %q", in, got, want)
		}
	}
}

// postgres_exporter's two start-up warnings in eru's probe-only setup are
// not quoted as the reason a probe fails.
func TestExporterStartupNoiseLeftOut(t *testing.T) {
	journal := `time=2026-10-15T06:00:00Z level=INFO source=main.go:80 msg="Starting postgres_exporter"
time=2026-10-15T06:00:00Z level=WARN source=main.go:81 msg="Error loading config" err="error opening config file \"\": open : no such file or directory"
time=2026-10-15T06:00:00Z level=WARN source=main.go:137 msg="Failed to create PostgresCollector" err="empty dsn"
time=2026-10-15T06:01:00Z level=ERROR source=probe.go:48 msg="Error opening connection to database" err="dial unix: no such file"
`
	got := lastExporterErrors(journal, 5)
	if len(got) != 1 || !strings.Contains(got[0], "Error opening connection") {
		t.Errorf("quoted %q", got)
	}
}
