package cli

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

type testJobError struct{ text string }

func (e *testJobError) Error() string { return e.text }

func TestJobEvidenceWarnings(t *testing.T) {
	var warnings jobEvidenceWarnings
	if got := warnings.warnings(); len(got) != 0 {
		t.Fatal("job without recording failures has warnings")
	}
	var forwarded []output.Event
	report := warnings.reporter(func(ev output.Event) { forwarded = append(forwarded, ev) })
	for range 3 {
		report(output.Event{Type: "warning", Code: "activity_not_recorded", Message: "DO-NOT-PROMOTE-PRIVATE-TEXT"})
	}
	if len(forwarded) != 3 {
		t.Fatal("warning tracking changed event forwarding")
	}
	got := warnings.warnings()
	if len(got) != 1 || got[0].Code != "activity_not_recorded" || strings.Contains(got[0].Message, "PRIVATE-TEXT") {
		t.Fatalf("unexpected result warnings: %+v", got)
	}
}

func TestJobEvidenceWarningsConcurrent(t *testing.T) {
	var warnings jobEvidenceWarnings
	report := warnings.reporter(nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			report(output.Event{Type: "warning", Code: "activity_not_recorded"})
			_ = warnings.warnings()
		})
	}
	wg.Wait()
	if len(warnings.warnings()) != 1 {
		t.Fatal("concurrent evidence failures were not summarized once")
	}
}

func TestJobEvidenceWarningsIgnoreUnrelatedEvents(t *testing.T) {
	var warnings jobEvidenceWarnings
	report := warnings.reporter(nil)
	report(output.Event{Type: "log", Code: "activity_not_recorded", Line: "ignored"})
	report(output.Event{Type: "warning", Code: "other_warning"})
	if len(warnings.warnings()) != 0 {
		t.Fatal("unrelated event became an activity warning")
	}
}

func TestWithEvidenceWarningsAttachesOnce(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "evidence was lost"}
	cause := errors.New("command failed")
	err := withEvidenceWarnings(&output.CLIError{
		Code:     output.CodeError,
		Message:  "job failed",
		Details:  map[string]any{"job_id": "J1"},
		ExitCode: output.ExitGeneric,
		Cause:    cause,
		Warnings: []output.Warning{warning},
	}, []output.Warning{warning})
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %v, want a typed failure", err)
	}
	if len(cliErr.Warnings) != 1 || cliErr.Warnings[0] != warning {
		t.Fatalf("warnings = %+v, want one deduplicated notice", cliErr.Warnings)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["job_id"] != "J1" {
		t.Fatalf("details changed: %#v", cliErr.Details)
	}
	if !errors.Is(err, cause) || output.ExitCode(err) != output.ExitGeneric {
		t.Fatalf("typed failure behavior changed: cause=%v exit=%d", errors.Is(err, cause), output.ExitCode(err))
	}
	untyped := &testJobError{text: "the final dump failed"}
	wrapped := withEvidenceWarnings(untyped, []output.Warning{warning})
	if !errors.Is(wrapped, untyped) {
		t.Fatalf("untyped error identity was lost: %v", wrapped)
	}
	var same *testJobError
	if !errors.As(wrapped, &same) || same != untyped {
		t.Fatalf("untyped error type was lost: %v", wrapped)
	}
	failure := output.Fail(nil, wrapped)
	if failure.Error.Code != output.CodeError || failure.Error.Message != untyped.Error() {
		t.Fatalf("untyped failure classification changed: %+v", failure.Error)
	}
	if len(failure.Warnings) != 1 || failure.Warnings[0] != warning {
		t.Fatalf("untyped failure lost its warning: %+v", failure.Warnings)
	}
}
