package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
)

func TestRestoreWindow(t *testing.T) {
	one := backup.Info{Sets: []backup.Set{{Label: "a", Stop: "2026-10-14T10:21:20Z"}}}
	two := backup.Info{Sets: []backup.Set{{Label: "a", Stop: "2026-10-14T08:00:00Z"}, {Label: "b", Stop: "2026-10-14T08:20:14Z"}}}
	cases := []struct {
		name     string
		info     backup.Info
		archived string
		from, to string
	}{
		{"archive time ends the window", two, "2026-10-14T08:35:15Z", "2026-10-14T08:00:01Z", "2026-10-14T08:35:15Z"},
		{"no archive time: the newest set's end", two, "", "2026-10-14T08:00:01Z", "2026-10-14T08:20:14Z"},
		{"archive older than the newest set", two, "2026-10-14T08:10:00Z", "2026-10-14T08:00:01Z", "2026-10-14T08:20:14Z"},
		{"one set, no archive time: never ends before it starts", one, "", "2026-10-14T10:21:21Z", "2026-10-14T10:21:21Z"},
		{"no sets", backup.Info{}, "", "", ""},
	}
	for _, c := range cases {
		w := restoreWindow(c.info, backup.ArchiveStatus{LastArchivedTime: c.archived})
		if w.From != c.from || w.To != c.to {
			t.Errorf("%s: window %s to %s, want %s to %s", c.name, w.From, w.To, c.from, c.to)
		}
	}
}

func TestFirstBackupWarning(t *testing.T) {
	missing := output.NewError(output.CodeInputRequired, "the bucket keys or the cipher pass are not on the node and were not supplied",
		output.ExitInput, "Run eru backup setup, or eru apply from a machine whose context has the recovery kit (eru login from a current transfer bundle).")
	stepErr := &output.CLIError{Code: output.CodeInputRequired, Message: "step pgbackrest-v1 failed: " + missing.Message, Hint: missing.Hint, Cause: missing}
	w := firstBackupWarning("app", stepErr)
	if w.Code != "first_backup_failed" {
		t.Errorf("code = %q", w.Code)
	}
	if strings.Contains(w.Message, "run eru apply to retry") {
		t.Errorf("input_required: %q suggests a bare eru apply", w.Message)
	}
	if !strings.Contains(w.Message, "recovery kit") {
		t.Errorf("input_required: %q lacks the step's hint", w.Message)
	}
	script := &output.CLIError{Code: output.CodeStepFailed, Message: "step pgbackrest-v1 failed: exit status 1", Hint: "Fix the cause and re-run apply. It resumes at this step.", Cause: errors.New("exit status 1")}
	if w := firstBackupWarning("app", script); !strings.HasSuffix(w.Message, "; run eru apply to retry") {
		t.Errorf("script failure: %q, want the eru apply retry", w.Message)
	}
}
