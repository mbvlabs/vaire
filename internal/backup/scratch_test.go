package backup

import (
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

func TestRestoreFilterDropsExtensionComment(t *testing.T) {
	var out []string
	f := &restoreFilter{out: func(l string) { out = append(out, l) }}
	for _, l := range []string{
		"pg_restore: while PROCESSING TOC:",
		"pg_restore: from TOC entry 4321; 0 0 COMMENT EXTENSION pg_stat_statements",
		"pg_restore: error: could not execute query: ERROR:  must be owner of extension pg_stat_statements",
		"Command was: COMMENT ON EXTENSION pg_stat_statements IS 'track planning and execution statistics of all SQL statements executed';",
		"pg_restore: warning: errors ignored on restore: 1",
	} {
		f.report(output.Event{Type: "log", Line: l})
	}
	f.flush()
	if f.errors != 1 || f.harmless != 1 {
		t.Fatalf("errors %d, harmless %d, want 1 and 1", f.errors, f.harmless)
	}
	if len(out) != 1 || slices.ContainsFunc(out, func(l string) bool { return l == "pg_restore: warning: errors ignored on restore: 1" }) {
		t.Fatalf("output %q, want only the explanation", out)
	}
}

func TestRestoreFilterKeepsOtherErrors(t *testing.T) {
	var out []string
	f := &restoreFilter{out: func(l string) { out = append(out, l) }}
	lines := []string{
		"pg_restore: from TOC entry 10; 1259 16390 TABLE t app_owner",
		"pg_restore: error: could not execute query: ERROR:  permission denied for schema x",
		"Command was: CREATE TABLE x.t (id integer);",
		"pg_restore: warning: errors ignored on restore: 1",
	}
	for _, l := range lines {
		f.report(output.Event{Type: "log", Line: l})
	}
	f.flush()
	if f.errors != 1 || f.harmless != 0 {
		t.Fatalf("errors %d, harmless %d, want 1 and 0", f.errors, f.harmless)
	}
	if !slices.Equal(out, lines) {
		t.Fatalf("output %q, want %q", out, lines)
	}
	if got := f.kept(lines); !slices.Equal(got, lines) {
		t.Fatalf("kept %q", got)
	}
}

func TestLiveDatabase(t *testing.T) {
	for name, want := range map[string]string{
		"app":                              "app",
		"app__restoring":                   "app",
		"app__drill":                       "app",
		"app__pre_restore_20261014T085103": "app",
		"my_app__restoring":                "my_app",
	} {
		if got := LiveDatabase(name); got != want {
			t.Errorf("LiveDatabase(%q) = %q, want %q", name, got, want)
		}
	}
}
