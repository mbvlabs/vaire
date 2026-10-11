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
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// progress renders node events and local phases while a command runs. With
// --stream it writes NDJSON events to stdout; in human mode it writes step
// lines to stderr, and log lines only with --verbose. Structured modes print
// nothing until the result.
type progress struct {
	opts   output.Options
	stdout io.Writer
	stderr io.Writer
}

func newProgress(cmd *cobra.Command) *progress {
	opts, _ := output.ParseOptions(cmd)
	return &progress{opts: opts, stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
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

// phase reports a local operator phase as a step event.
func (p *progress) phase(name, state, detail string) {
	p.event(output.Event{Type: "step", Step: name, State: state, Detail: detail})
}

// note prints one informational line in human mode, or a log event when
// streaming.
func (p *progress) note(line string) {
	if p.opts.Stream {
		p.event(output.Event{Type: "log", Step: "eru", Line: line})
		return
	}
	if p.human() {
		_, _ = fmt.Fprintln(p.stderr, secrets.Redact(line))
	}
}

// promptLine asks for one line with a default. It never prompts in
// structured mode or without a TTY.
func promptLine(cmd *cobra.Command, question, def string) (string, error) {
	if MustNotPrompt(cmd) {
		return def, nil
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
