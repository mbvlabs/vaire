package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
)

func TestProgressPreservesEvidenceWarningsInResult(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	for _, mode := range []string{"json", "agent", "human"} {
		t.Run(mode, func(t *testing.T) {
			cmd := commandAt(t, "db credentials", "--output", mode)
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			p := newProgress(cmd)
			// Some callers intentionally ignore the RPC reply. The event still
			// has to survive in the final structured result, not just stderr.
			p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
			p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
			if err := output.OK(cmd, map[string]bool{"rotated": true}, "rotated"); err != nil {
				t.Fatal(err)
			}
			if mode == "human" {
				if count := strings.Count(stderr.String(), warning.Message); count != 1 || strings.Contains(stdout.String(), warning.Message) {
					t.Fatalf("warning printed %d times in the result: stdout=%q stderr=%q", count, stdout.String(), stderr.String())
				}
				return
			}
			var env output.Envelope
			if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if len(env.Warnings) != 1 || env.Warnings[0] != warning {
				t.Fatalf("warnings = %+v, want exactly %+v", env.Warnings, warning)
			}
			if stderr.Len() != 0 {
				t.Fatalf("structured output wrote stderr: %q", stderr.String())
			}
		})
	}
}

func TestProgressHumanPrintsEachWarningOnce(t *testing.T) {
	first := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	second := output.Warning{Code: "doctor_check_failed", Message: "the doctor check failed"}
	for _, mode := range []string{"plain", "rich"} {
		t.Run(mode, func(t *testing.T) {
			cmd := commandAt(t, "db credentials")
			var stdout bytes.Buffer
			p := &progress{opts: output.Options{Mode: output.ModeHuman}, cmd: cmd, stdout: &stdout}
			var rendered func() string
			if mode == "rich" {
				file, err := os.CreateTemp(t.TempDir(), "live")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ui.Close()
					_ = file.Close()
				})
				p.live = ui.Start(file)
				p.stderr = file
				rendered = func() string {
					raw, err := os.ReadFile(file.Name())
					if err != nil {
						t.Fatal(err)
					}
					return string(raw)
				}
			} else {
				var stderr bytes.Buffer
				p.stderr = &stderr
				rendered = func() string { return stderr.String() }
			}
			// The node can deliver the same notice twice; both distinct notices
			// must still appear.
			for _, warning := range []output.Warning{first, second, first, first, second} {
				p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
			}
			out := rendered()
			if count := strings.Count(out, first.Message); count != 1 {
				t.Fatalf("first warning printed %d times: %q", count, out)
			}
			if count := strings.Count(out, second.Message); count != 1 {
				t.Fatalf("second warning printed %d times: %q", count, out)
			}
		})
	}
}

func TestProgressHumanKeepsDistinctWarningIdentities(t *testing.T) {
	const sameText = "the same text means two different notices"
	const sameCode = "the same code still means another notice"
	cmd := commandAt(t, "db credentials")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	p := newProgress(cmd)
	for _, warning := range []output.Warning{
		{Code: "first", Message: sameText},
		{Code: "second", Message: sameText},
		{Code: "first", Message: sameCode},
		{Code: "first", Message: sameText},
	} {
		p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	}
	if count := strings.Count(stderr.String(), sameText); count != 2 {
		t.Fatalf("warnings with the same text printed %d times: %q", count, stderr.String())
	}
	if count := strings.Count(stderr.String(), sameCode); count != 1 {
		t.Fatalf("warning with the same code printed %d times: %q", count, stderr.String())
	}
}

func TestProgressStreamKeepsWarningEventsAndOneResultWarning(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--json", "--stream")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	if err := output.OK(cmd, nil, "rotated"); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&stdout)
	warningEvents := 0
	for {
		var event struct {
			Type     string          `json:"type"`
			Envelope output.Envelope `json:"envelope"`
		}
		if err := dec.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		switch event.Type {
		case "warning":
			warningEvents++
		case "result":
			if len(event.Envelope.Warnings) != 1 || event.Envelope.Warnings[0] != warning {
				t.Fatalf("terminal event warnings = %+v, want exactly %+v", event.Envelope.Warnings, warning)
			}
		}
	}
	if warningEvents == 0 {
		t.Fatal("stream lost the warning event")
	}
}

func TestProgressHumanPrintsReplayedResultWarningOnce(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "job follow", "J1")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	p := newProgress(cmd)
	// A follower replays the job's journal warning and the result carries it
	// too; the human must see it once.
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	if err := output.Success(cmd, output.Result{Message: "job J1 succeeded", Warnings: []output.Warning{warning}}); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(stderr.String(), warning.Message); count != 1 {
		t.Fatalf("warning printed %d times: stdout=%q stderr=%q", count, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), warning.Message) {
		t.Fatalf("result repeated the shown warning: %q", stdout.String())
	}
}

func TestProgressQuietHumanKeepsWarningsOut(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--quiet")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	if err := output.OK(cmd, nil, "rotated"); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("quiet output changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestProgressWarningMergeAndReset(t *testing.T) {
	cmd := commandAt(t, "db credentials", "--json")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	if err := output.Success(cmd, output.Result{Warnings: []output.Warning{warning}}); err != nil {
		t.Fatal(err)
	}
	var env output.Envelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Warnings) != 1 {
		t.Fatalf("duplicated explicit and observed warnings: %+v", env.Warnings)
	}
	stdout.Reset()
	newProgress(cmd)
	if err := output.OK(cmd, nil, "next invocation"); err != nil {
		t.Fatal(err)
	}
	var next output.Envelope
	if err := json.Unmarshal(stdout.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Warnings) != 0 {
		t.Fatalf("warning leaked into a new invocation: %+v", next.Warnings)
	}
}

func TestProgressEvidenceWarningSurvivesJQAndStream(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	t.Run("jq", func(t *testing.T) {
		cmd := commandAt(t, "db credentials", "--jq", ".warnings")
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		p := newProgress(cmd)
		p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
		if err := output.OK(cmd, nil, "rotated"); err != nil {
			t.Fatal(err)
		}
		var warnings []output.Warning
		if err := json.Unmarshal(stdout.Bytes(), &warnings); err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || warnings[0] != warning {
			t.Fatalf("filtered warnings = %+v", warnings)
		}
	})
	t.Run("stream", func(t *testing.T) {
		cmd := commandAt(t, "db credentials", "--json", "--stream")
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		p := newProgress(cmd)
		p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
		if err := output.OK(cmd, nil, "rotated"); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(&stdout)
		var first output.Event
		if err := dec.Decode(&first); err != nil || first.Type != "warning" {
			t.Fatalf("first event = %+v, err = %v", first, err)
		}
		var last struct {
			Type     string          `json:"type"`
			Envelope output.Envelope `json:"envelope"`
		}
		if err := dec.Decode(&last); err != nil {
			t.Fatal(err)
		}
		if last.Type != "result" || len(last.Envelope.Warnings) != 1 || last.Envelope.Warnings[0] != warning {
			t.Fatalf("terminal event lost warning: %+v", last)
		}
	})
}

// failureEnvelope renders one failed command and decodes its structured
// envelope. It keeps the test focused on the warning contract rather than on
// the error type that produced it.
func failureEnvelope(t *testing.T, cmd *bytes.Buffer) output.Failure {
	t.Helper()
	var failure output.Failure
	if decodeErr := json.Unmarshal(cmd.Bytes(), &failure); decodeErr != nil {
		t.Fatalf("failure does not decode: %v (%q)", decodeErr, cmd.String())
	}
	return failure
}

func TestFailureEnvelopeCarriesObservedEvidenceWarnings(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	for _, mode := range []string{"json", "agent"} {
		t.Run(mode, func(t *testing.T) {
			cmd := commandAt(t, "db credentials", "--output", mode)
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			p := newProgress(cmd)
			p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
			err := output.RenderError(cmd, &output.CLIError{
				Code:     output.CodePrecondition,
				Message:  "the operation failed after it started",
				Hint:     "read the node history",
				Details:  map[string]any{"reason": "activity_unavailable"},
				ExitCode: output.ExitPrecondition,
			})
			if err != nil {
				t.Fatal(err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("structured output wrote stderr: %q", stderr.String())
			}
			failure := failureEnvelope(t, &stdout)
			if failure.OK || failure.Error.Code != output.CodePrecondition ||
				failure.Error.Message != "the operation failed after it started" ||
				failure.Error.Hint != "read the node history" {
				t.Fatalf("failure lost its error fields: %+v", failure)
			}
			details, _ := failure.Error.Details.(map[string]any)
			if details["reason"] != "activity_unavailable" {
				t.Fatalf("failure lost error details: %#v", failure.Error.Details)
			}
			if len(failure.Warnings) != 1 || failure.Warnings[0] != warning {
				t.Fatalf("warnings = %+v, want exactly %+v", failure.Warnings, warning)
			}
		})
	}
}

func TestFailureEnvelopeDeduplicatesReplyAndObservedWarnings(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--json")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	err := output.RenderError(cmd, &output.CLIError{
		Code:     output.CodeError,
		Message:  "failed",
		ExitCode: output.ExitGeneric,
		Warnings: []output.Warning{warning},
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := failureEnvelope(t, &stdout)
	if len(failure.Warnings) != 1 {
		t.Fatalf("duplicated reply and observed warning: %+v", failure.Warnings)
	}
}

func TestFailureEnvelopeCarriesJobEvidenceWithoutEvents(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "the job's evidence could not be recorded"}
	cmd := commandAt(t, "db credentials", "--json")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	// No warning event was streamed: the failed follower carries the notice on
	// the typed error itself.
	err := output.RenderError(cmd, &output.CLIError{
		Code:     output.CodeError,
		Message:  "job failed",
		ExitCode: output.ExitGeneric,
		Warnings: []output.Warning{warning},
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := failureEnvelope(t, &stdout)
	if len(failure.Warnings) != 1 || failure.Warnings[0] != warning {
		t.Fatalf("warnings = %+v, want the job's evidence warning", failure.Warnings)
	}
}

func TestFailureStreamCarriesWarningsInResultEvent(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--json", "--stream")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	err := output.RenderError(cmd, &output.CLIError{Code: output.CodeError, Message: "failed", ExitCode: output.ExitGeneric})
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(&stdout)
	var first output.Event
	if decodeErr := dec.Decode(&first); decodeErr != nil || first.Type != "warning" {
		t.Fatalf("first event = %+v, err = %v", first, decodeErr)
	}
	var last struct {
		Type     string         `json:"type"`
		Envelope output.Failure `json:"envelope"`
	}
	if decodeErr := dec.Decode(&last); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if last.Type != "result" || len(last.Envelope.Warnings) != 1 || last.Envelope.Warnings[0] != warning {
		t.Fatalf("terminal failure event lost warning: %+v", last)
	}
}

func TestFailureHumanPrintsEachWarningOnce(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	p := newProgress(cmd)
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	p.event(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	if err := output.RenderError(cmd, &output.CLIError{Code: output.CodeError, Message: "failed", ExitCode: output.ExitGeneric}); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(stderr.String(), warning.Message); count != 1 {
		t.Fatalf("warning printed %d times: %q", count, stderr.String())
	}

	stderr.Reset()
	cmd = commandAt(t, "db credentials")
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := output.RenderError(cmd, &output.CLIError{
		Code:     output.CodeError,
		Message:  "failed",
		ExitCode: output.ExitGeneric,
		Warnings: []output.Warning{warning},
	}); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(stderr.String(), warning.Message); count != 1 {
		t.Fatalf("unshown warning printed %d times: %q", count, stderr.String())
	}
}

func TestFailureQuietHumanKeepsWarningOut(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--quiet")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := output.RenderError(cmd, &output.CLIError{
		Code:     output.CodeError,
		Message:  "failed",
		ExitCode: output.ExitGeneric,
		Warnings: []output.Warning{warning},
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), warning.Message) {
		t.Fatalf("quiet human output printed a warning: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed") {
		t.Fatalf("quiet human output lost the error: %q", stderr.String())
	}
}

func TestFailureJQCanSelectWarnings(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "activity completion could not be recorded"}
	cmd := commandAt(t, "db credentials", "--jq", ".warnings")
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	if err := output.RenderError(cmd, &output.CLIError{
		Code:     output.CodeError,
		Message:  "failed",
		ExitCode: output.ExitGeneric,
		Warnings: []output.Warning{warning},
	}); err != nil {
		t.Fatal(err)
	}
	var warnings []output.Warning
	if err := json.Unmarshal(stdout.Bytes(), &warnings); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != warning {
		t.Fatalf("filtered warnings = %+v", warnings)
	}
}

func TestNodeRunUntypedFailureCarriesEvidenceWarning(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "the job's evidence could not be recorded"}
	untyped := errors.New("creating the final dump failed: permission denied")
	wrapped := withEvidenceWarnings(untyped, []output.Warning{warning})
	if !errors.Is(wrapped, untyped) {
		t.Fatal("untyped error identity was not preserved")
	}
	if output.ExitCode(wrapped) != output.ExitGeneric {
		t.Fatalf("exit code = %d, want generic classification", output.ExitCode(wrapped))
	}
	if wrapped.Error() != untyped.Error() {
		t.Fatalf("error text changed: %q", wrapped.Error())
	}

	for _, mode := range []string{"json", "agent"} {
		t.Run(mode, func(t *testing.T) {
			cmd := commandAt(t, "node run", "--output", mode)
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			if err := output.RenderError(cmd, wrapped); err != nil {
				t.Fatal(err)
			}
			failure := failureEnvelope(t, &stdout)
			if failure.OK || failure.Error.Code != output.CodeError || failure.Error.Message != untyped.Error() {
				t.Fatalf("untyped failure classification changed: %+v", failure.Error)
			}
			if len(failure.Warnings) != 1 || failure.Warnings[0] != warning {
				t.Fatalf("warnings = %+v, want %+v", failure.Warnings, warning)
			}
			if stderr.Len() != 0 {
				t.Fatalf("structured output wrote stderr: %q", stderr.String())
			}
		})
	}

	t.Run("human", func(t *testing.T) {
		cmd := commandAt(t, "node run")
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := output.RenderError(cmd, wrapped); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "Error: "+untyped.Error()) {
			t.Fatalf("human output lost the original error: %q", stderr.String())
		}
		if !strings.Contains(stderr.String(), "warning: "+warning.Message) {
			t.Fatalf("human output lost the evidence warning: %q", stderr.String())
		}
	})
}
