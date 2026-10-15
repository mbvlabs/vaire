package node

import (
	"strings"
	"testing"
)

func TestParsePGUp(t *testing.T) {
	up := parsePGUp(strings.NewReader("# HELP pg_up Whether the last scrape of metrics from PostgreSQL was able to connect to the server (1 for yes, 0 for no).\n# TYPE pg_up gauge\npg_up 1\n"))
	if !up.Known || !up.Up {
		t.Errorf("pg_up 1: %+v", up)
	}
	down := parsePGUp(strings.NewReader("pg_exporter_last_scrape_error 1\npg_up 0\n"))
	if !down.Known || down.Up {
		t.Errorf("pg_up 0: %+v", down)
	}
	if none := parsePGUp(strings.NewReader("go_goroutines 7\n")); none.Known || none.Error == "" {
		t.Errorf("no pg_up: %+v", none)
	}
}

func TestLastExporterErrors(t *testing.T) {
	journal := `time=2026-10-15T10:00:00Z level=INFO source=probe.go:41 msg="no auth_module specified, using default"
time=2026-10-15T10:00:01Z level=ERROR source=datasource.go:54 msg="Error opening connection to database" err="invalid URL escape \"%2F\""
time=2026-10-15T10:00:02Z level=INFO source=probe.go:41 msg="no auth_module specified, using default"
`
	got := lastExporterErrors(journal, 5)
	if len(got) != 1 || !strings.Contains(got[0], "invalid URL escape") {
		t.Errorf("got %q", got)
	}
	if got := lastExporterErrors("a\nb\nc\n", 2); strings.Join(got, ",") != "b,c" {
		t.Errorf("no errors: got %q, want the last lines", got)
	}
}
