package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/remote"
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
	cmd    *cobra.Command
	stdout io.Writer
	stderr io.Writer
	// live is the terminal UI, nil for plain output.
	live *ui.Live
	// hidden holds warning codes the human output leaves out because the
	// command's result already says the same.
	hidden map[string]bool
	// status is the terminal UI's waiting line while wait runs.
	status *ui.Status
	// beat is the plain output's heartbeat while wait runs.
	beat *heartbeat
}

// hideWarning leaves warnings with code out of the human output. JSON and
// --stream output keep them.
func (p *progress) hideWarning(code string) {
	if p.hidden == nil {
		p.hidden = map[string]bool{}
	}
	p.hidden[code] = true
}

func newProgress(cmd *cobra.Command) *progress {
	opts, _ := output.ParseOptions(cmd)
	p := &progress{opts: opts, cmd: cmd, stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
	output.TrackShownWarnings(cmd)
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
	p.track(ev)
	if ev.Type == "warning" && ev.Code != "" && p.hidden[ev.Code] {
		p.shown(ev.Message)
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
		p.shown(ev.Message)
		_, _ = fmt.Fprintln(p.stderr, "warning: "+secrets.Redact(ev.Message))
	case "progress":
		_, _ = fmt.Fprintf(p.stderr, "  %s %s\n", ev.Phase, ev.Detail)
	}
}

// heartbeatEvery is how long plain output may stay silent while waiting
// before a "still ..." line says the command is alive.
const heartbeatEvery = 30 * time.Second

// wait shows that the command is waiting for the node until the returned
// func is called. The terminal UI draws a status line with a spinner, the
// current phase and its elapsed time once nothing was printed for 300ms;
// plain human output prints a "… still <phase> (2m10s)" line after
// heartbeatEvery of silence. Structured, --stream and quiet modes show
// nothing. A wait inside a wait keeps the outer one. The returned func may
// be called more than once.
func (p *progress) wait(label string) func() {
	var once sync.Once
	stop := p.startWait(label)
	return func() { once.Do(stop) }
}

func (p *progress) startWait(label string) func() {
	switch {
	case p.live != nil:
		if p.status != nil {
			return func() {}
		}
		st, stop := p.live.Wait(label)
		p.status = st
		return func() {
			stop()
			p.status = nil
		}
	case p.human():
		if p.beat != nil {
			return func() {}
		}
		h := &heartbeat{w: p.stderr, label: label, every: heartbeatEvery}
		p.beat, p.stderr = h, h
		stop := h.start()
		return func() {
			stop()
			p.beat, p.stderr = nil, h.w
		}
	}
	return func() {}
}

// during runs fn with the waiting line showing label.
func (p *progress) during(label string, fn func() error) error {
	defer p.wait(label)()
	return fn()
}

// waitOn is wait for code that has no progress of its own, such as tool
// downloads and eru upgrade. It shares the process's live area.
func waitOn(cmd *cobra.Command, label string) func() {
	if cmd == nil {
		return func() {}
	}
	opts, _ := output.ParseOptions(cmd)
	p := &progress{opts: opts, cmd: cmd, stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
	if p.human() && richOutput(cmd) {
		p.live = ui.Start(os.Stderr)
	}
	return p.wait(label)
}

// call runs one node RPC with the waiting line up, and the progress's event
// renderer when opts has none.
func (p *progress) call(ctx context.Context, c remote.Caller, method string, params any, opts remote.CallOptions) (remote.Reply, error) {
	defer p.wait(waitLabel(method, params))()
	if opts.OnEvent == nil {
		opts.OnEvent = p.event
	}
	return c.Call(ctx, method, params, opts)
}

// track feeds an event to the waiting line: a started step becomes the
// phase until it ends, log and progress lines its detail.
func (p *progress) track(ev output.Event) {
	if p.status == nil && p.beat == nil {
		return
	}
	switch ev.Type {
	case "step":
		phase := ""
		if ev.State == "started" {
			phase, _ = phaseTitle(ev.Step)
		}
		p.setPhase(phase)
	case "log":
		p.setDetail(secrets.Redact(ev.Line))
	case "progress":
		if ev.Phase != "job" {
			p.setDetail(secrets.Redact(strings.TrimSpace(ev.Phase + " " + ev.Detail)))
		}
	}
}

func (p *progress) setPhase(phase string) {
	if p.status != nil {
		p.status.Phase(phase)
	}
	if p.beat != nil {
		p.beat.phaseTo(phase)
	}
}

func (p *progress) setDetail(detail string) {
	if p.status != nil {
		p.status.Detail(detail)
	}
}

// heartbeat is the plain output's sign of life while waiting: it wraps
// stderr to see when the command last printed, and after every of silence
// prints what it still waits for.
type heartbeat struct {
	mu           sync.Mutex
	w            io.Writer
	every        time.Duration
	label, phase string
	started      time.Time
	phaseStarted time.Time
	last         time.Time
}

func (h *heartbeat) Write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = time.Now()
	return h.w.Write(b)
}

func (h *heartbeat) phaseTo(phase string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if phase != h.phase {
		h.phase, h.phaseStarted = phase, time.Now()
	}
}

// start begins the heartbeat; the returned func stops it.
func (h *heartbeat) start() func() {
	now := time.Now()
	h.started, h.phaseStarted, h.last = now, now, now
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				h.mu.Lock()
				if now.Sub(h.last) >= h.every {
					_, _ = fmt.Fprintln(h.w, h.line(now))
					h.last = now
				}
				h.mu.Unlock()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// line is the heartbeat text. The caller holds h.mu.
func (h *heartbeat) line(now time.Time) string {
	what, took := h.label, now.Sub(h.started).Round(time.Second).String()
	if h.phase != "" {
		what = h.phase
	}
	if h.phaseStarted.After(h.started) {
		took = now.Sub(h.phaseStarted).Round(time.Second).String() + ", " + took + " total"
	}
	return "… still " + what + " (" + took + ")"
}

// waitLabel is what the waiting line says for an RPC method: "restoring
// app2", "backing up app2", or "waiting for <method>".
func waitLabel(method string, params any) string {
	var named struct {
		Database string `json:"database"`
		Name     string `json:"name"`
		Source   string `json:"source"`
		ID       string `json:"id"`
	}
	if raw, err := json.Marshal(params); err == nil {
		_ = json.Unmarshal(raw, &named)
	}
	db := named.Database
	if db == "" {
		db = named.Name
	}
	with := func(verb, obj string) string {
		if obj == "" {
			return verb
		}
		return verb + " " + obj
	}
	switch method {
	case "apply":
		return "applying"
	case "plan":
		return "planning"
	case "db.list":
		return "listing the databases"
	case "db.credentials":
		return with("rotating the credentials of", db)
	case "backup.list":
		return "listing the backups"
	case "job.list":
		return "listing the jobs"
	case "tune.show":
		return "reading the tuning"
	case "restore.status":
		return "reading the restore status"
	case "restore.cleanup":
		return with("cleaning up the restore of", db)
	case "cluster.bootstrap":
		return "running the setup steps"
	case "cluster.confirm-secrets":
		return "confirming the secrets handoff"
	case "ssh.lockdown":
		return "closing public SSH"
	case "ssh.confirm":
		return "confirming the SSH lockdown"
	case "ssh.status":
		return "reading the SSH state"
	case "tls.status":
		return "checking the certificate"
	case "config.get":
		return "reading the config"
	case "config.set":
		return "updating the config"
	case "tune.apply":
		return "applying the tuning"
	case "db.create":
		return with("creating", db)
	case "db.resize":
		return with("resizing", db)
	case "db.drop":
		return with("dropping", db)
	case "db.fork":
		return with("forking", named.Source)
	case "dump.create":
		return with("dumping", db)
	case "restore.start":
		return with("restoring", db)
	case "restore.promote":
		return with("promoting the restore of", db)
	case "restore.abort":
		return with("aborting the restore of", db)
	case "restore.undo":
		return with("undoing the restore of", db)
	case "restore.drill":
		return with("running a restore drill of", db)
	case "backup.now":
		return with("backing up", db)
	case "backup.verify":
		return with("verifying the backups of", db)
	case "job.follow":
		return with("following job", named.ID)
	}
	return "waiting for " + method
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
		p.shown(ev.Message)
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
	case "init.master-backup":
		return "First master bundle", "Master backup"
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
		p.shown(msg)
		p.live.Println(ui.Warn(secrets.Redact(msg)))
	case p.human():
		p.shown(msg)
		_, _ = fmt.Fprintln(p.stderr, "warning: "+secrets.Redact(msg))
	}
}

// shown records a warning printed while the command ran: the node sends each
// result warning as an event before the result, and the human result then
// leaves it out instead of printing it again.
func (p *progress) shown(msg string) {
	output.MarkWarningShown(p.cmd, msg)
}

// panel prints a titled block, boxed on a terminal. Lines inside stay plain
// so they copy cleanly.
func (p *progress) panel(title string, lines []string) {
	switch {
	case p.opts.Stream:
		p.event(output.Event{Type: "log", Step: "eru", Line: title + ": " + strings.Join(lines, "; ")})
	case p.live != nil:
		p.live.Println(ui.PanelFit(title, strings.Join(lines, "\n"), p.live.Width()))
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
