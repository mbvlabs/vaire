package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// progress renders node events and local phases while a command runs. With
// --stream it writes NDJSON events to stdout. In human mode on a terminal it
// draws spinners, headers and panels (internal/cli/ui); otherwise it writes
// step lines to stderr, and log lines only with --verbose. Structured modes
// print nothing until the result.
type progress struct {
	opts   output.Options
	stdout io.Writer
	stderr io.Writer
	// live is the terminal UI, nil for plain output.
	live *ui.Live
}

func newProgress(cmd *cobra.Command) *progress {
	opts, _ := output.ParseOptions(cmd)
	p := &progress{opts: opts, stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
	if p.human() && richOutput(cmd) {
		p.live = ui.Start(os.Stderr)
	}
	return p
}

// richOutput reports whether this invocation draws the terminal UI: human
// mode, stderr is the process's terminal, and --no-input isn't set.
func richOutput(cmd *cobra.Command) bool {
	opts, err := output.ParseOptions(cmd)
	if err != nil || output.UsesStructuredOutput(opts) || opts.Quiet {
		return false
	}
	if flag := cmd.Flags().Lookup("no-input"); flag != nil && flag.Value.String() == "true" {
		return false
	}
	if cmd.ErrOrStderr() != os.Stderr {
		return false
	}
	return ui.Enabled(os.Stderr)
}

func (p *progress) human() bool {
	return !p.opts.Stream && p.opts.Mode == output.ModeHuman && p.opts.JQ == "" && !p.opts.Quiet
}

// event renders one node or local event.
func (p *progress) event(ev output.Event) {
	if p.opts.Stream {
		ev.Schema = output.EventSchema
		if ev.TS == "" {
			ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
		}
		raw, err := json.Marshal(secrets.RedactValue(ev))
		if err == nil {
			_, _ = fmt.Fprintln(p.stdout, string(raw))
		}
		return
	}
	if !p.human() {
		return
	}
	if p.live != nil {
		p.liveEvent(ev)
		return
	}
	switch ev.Type {
	case "step":
		prefix := "  "
		if ev.Index != nil && ev.Total != nil {
			prefix = fmt.Sprintf("  [%d/%d] ", *ev.Index, *ev.Total)
		}
		line := prefix + ev.Step + " " + ev.State
		if ev.Detail != "" && ev.State != "skipped" {
			line += ": " + ev.Detail
		} else if ev.State == "skipped" && ev.Detail != "" {
			line += " (" + ev.Detail + ")"
		}
		_, _ = fmt.Fprintln(p.stderr, secrets.Redact(line))
	case "log":
		if p.opts.Verbose {
			_, _ = fmt.Fprintln(p.stderr, "      "+secrets.Redact(ev.Line))
		}
	case "warning":
		_, _ = fmt.Fprintln(p.stderr, "warning: "+secrets.Redact(ev.Message))
	case "progress":
		_, _ = fmt.Fprintf(p.stderr, "  %s %s\n", ev.Phase, ev.Detail)
	}
}

// liveEvent draws one event in the terminal UI.
func (p *progress) liveEvent(ev output.Event) {
	l := p.live
	switch ev.Type {
	case "step":
		p.liveStep(ev)
	case "log":
		line := secrets.Redact(ev.Line)
		if p.opts.Verbose {
			l.Println(ui.Dim("    " + line))
		}
		if t := l.Task(ev.Step); t != nil {
			t.Detail(line)
		}
	case "warning":
		l.Println(ui.Warn(secrets.Redact(ev.Message)))
	case "progress":
		if t := l.Task(ev.Step); t != nil {
			pct := 0.0
			if ev.Pct != nil {
				pct = float64(*ev.Pct) / 100
			}
			t.Progress(pct, strings.TrimSpace(ev.Phase+" "+ev.Detail))
		}
	}
}

func (p *progress) liveStep(ev output.Event) {
	l := p.live
	title, header := phaseTitle(ev.Step)
	counted := ev.Index != nil && ev.Total != nil
	if counted {
		title = fmt.Sprintf("%s %s", ui.Dim(fmt.Sprintf("[%d/%d]", *ev.Index, *ev.Total)), ev.Step)
	}
	detail := secrets.Redact(ev.Detail)
	switch ev.State {
	case "started":
		if header != "" {
			l.Header(header)
		}
		t := l.Start(ev.Step, title)
		t.Detail(detail)
		if counted {
			t.Progress(float64(*ev.Index-1)/float64(*ev.Total), fmt.Sprintf("step %d of %d", *ev.Index, *ev.Total))
		}
	case "completed":
		p.endTask(ev.Step, title, ui.EndOK, detail)
	case "failed":
		p.endTask(ev.Step, title, ui.EndFail, detail)
	case "skipped":
		if l.Running(ev.Step) {
			p.endTask(ev.Step, title, ui.EndSkip, detail)
			return
		}
		if detail != "" {
			detail = "(" + detail + ")"
		}
		l.Line(ui.EndSkip, title, detail)
	default:
		l.Println(ui.Note(title + " " + ev.State + " " + detail))
	}
}

func (p *progress) endTask(key, title, state, detail string) {
	l := p.live
	if !l.Running(key) {
		l.Line(state, title, detail)
		return
	}
	l.Task(key).End(state, detail)
}

// phaseTitle names a local phase for humans and gives the section header it
// belongs to. Node step IDs come back unchanged without a header.
func phaseTitle(step string) (title, header string) {
	switch step {
	case "init.secrets":
		return "Generate cluster secrets", "Prepare"
	case "init.first-contact":
		return "First contact over SSH", "Host"
	case "init.binary":
		return "Install eru on the server", "Host"
	case "init.bootstrap":
		return "Harden the host", "Host"
	case "init.transport":
		return "Switch to WireGuard", "Lockdown"
	case "init.lockdown":
		return "Close public SSH", "Lockdown"
	case "init.dns":
		return "DNS records", "DNS"
	case "init.database":
		return "Set up PostgreSQL and PgBouncer", "Database"
	case "init.tls":
		return "Certificate", "TLS"
	case "init.backups":
		return "Turn on backups", "Backups"
	case "init.secrets-document":
		return "Secrets handoff", "Secrets"
	}
	return step, ""
}

// phase reports a local operator phase as a step event.
func (p *progress) phase(name, state, detail string) {
	p.event(output.Event{Type: "step", Step: name, State: state, Detail: detail})
}

// task returns the running live task for a phase, or nil in plain mode.
func (p *progress) task(name string) *ui.Task {
	if p.live == nil || !p.live.Running(name) {
		return nil
	}
	return p.live.Task(name)
}

// note prints one informational line in human mode, or a log event when
// streaming.
func (p *progress) note(line string) {
	if p.opts.Stream {
		p.event(output.Event{Type: "log", Step: "eru", Line: line})
		return
	}
	if p.live != nil {
		p.live.Println(ui.Note(secrets.Redact(line)))
		return
	}
	if p.human() {
		_, _ = fmt.Fprintln(p.stderr, secrets.Redact(line))
	}
}

// warn prints a warning line now. Warnings that belong in the result are
// still added to its envelope by the caller.
func (p *progress) warn(msg string) {
	switch {
	case p.opts.Stream:
		p.event(output.Event{Type: "warning", Message: msg})
	case p.live != nil:
		p.live.Println(ui.Warn(secrets.Redact(msg)))
	case p.human():
		_, _ = fmt.Fprintln(p.stderr, "warning: "+secrets.Redact(msg))
	}
}

// panel prints a titled block, boxed on a terminal. Lines inside stay plain
// so they copy cleanly.
func (p *progress) panel(title string, lines []string) {
	switch {
	case p.opts.Stream:
		p.event(output.Event{Type: "log", Step: "eru", Line: title + ": " + strings.Join(lines, "; ")})
	case p.live != nil:
		p.live.Println(ui.Panel(title, strings.Join(lines, "\n")))
	case p.human():
		_, _ = fmt.Fprintln(p.stderr, title)
		for _, l := range lines {
			_, _ = fmt.Fprintln(p.stderr, "  "+l)
		}
	}
}

// promptLine asks for one line with a default. It never prompts in
// structured mode or without a TTY.
func promptLine(cmd *cobra.Command, question, def string) (string, error) {
	if MustNotPrompt(cmd) {
		return def, nil
	}
	if richOutput(cmd) {
		v, err := ui.Input(question, "", def)
		if err != nil {
			return "", promptErr(err)
		}
		if v == "" {
			return def, nil
		}
		return v, nil
	}
	label := question
	if def != "" {
		label += " [" + def + "]"
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), label+": "); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// readSecretTTY reads a value from the terminal without echo.
func readSecretTTY(cmd *cobra.Command, label string) (string, error) {
	if richOutput(cmd) {
		v, err := ui.Secret(label)
		if err != nil {
			return "", output.WrapError(output.CodeInputRequired, promptErr(err), output.ExitInput, "")
		}
		return v, nil
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), label+": "); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // G115: file descriptors fit in int.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", output.WrapError(output.CodeInputRequired, err, output.ExitInput, "")
	}
	return strings.TrimSpace(string(raw)), nil
}

// typeWord asks the operator to type word, as in "Type 'stored' once both are
// stored safely". It loops until the word is typed; an empty answer without a
// terminal fails.
func typeWord(cmd *cobra.Command, question, description, word string) error {
	if richOutput(cmd) {
		return promptErr(ui.TypeWord(question, description, word))
	}
	if description != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), description)
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	for {
		if _, err := fmt.Fprint(cmd.ErrOrStderr(), question+": "); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) == word {
			return nil
		}
		if err != nil {
			return output.NewError(output.CodeInputRequired, "expected "+word+" to be typed", output.ExitInput, "")
		}
	}
}

// promptErr turns a cancelled prompt into an input_required error.
func promptErr(err error) error {
	if errors.Is(err, ui.ErrAborted) {
		return output.NewError(output.CodeInputRequired, "the prompt was cancelled", output.ExitInput, "")
	}
	return err
}

// readFirstLine reads the first line of r, for --*-stdin flags.
func readFirstLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 64*1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func stringFlag(cmd *cobra.Command, name string) string {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return strings.TrimSpace(flag.Value.String())
	}
	return ""
}

// renderRichError draws a failure as an error block: the message, the failed
// step, the last lines of its output and the hint.
func renderRichError(w io.Writer, failure output.Failure) bool {
	block := ui.ErrorBlock{Code: failure.Error.Code, Message: failure.Error.Message, Hint: failure.Error.Hint}
	if details, ok := failure.Error.Details.(map[string]any); ok {
		if step, ok := details["step"].(string); ok {
			block.Step = step
		}
		switch tail := details["output_tail"].(type) {
		case []string:
			block.Tail = tail
		case []any:
			for _, line := range tail {
				if s, ok := line.(string); ok {
					block.Tail = append(block.Tail, s)
				}
			}
		}
	}
	width := 80
	if cols, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && cols > 0 { //nolint:gosec // G115: file descriptors fit in int.
		width = cols
	}
	_, err := fmt.Fprintln(ui.Writer(w), "\n"+block.Render(width))
	return err == nil
}
