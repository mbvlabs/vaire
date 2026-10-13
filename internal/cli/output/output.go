package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itchyny/gojq"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Mode is the selected output format.
type Mode string

const (
	ModeHuman    Mode = "human"
	ModeJSON     Mode = "json"
	ModeAgent    Mode = "agent"
	ModeMarkdown Mode = "markdown"
)

// Options are the persistent output flags for one invocation.
type Options struct {
	Mode    Mode
	Quiet   bool
	JQ      string
	Verbose bool
	Stream  bool
}

// Warning is a non-fatal notice carried on a success envelope.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Envelope is the success body for json and agent output.
type Envelope struct {
	Schema   string    `json:"schema"`
	OK       bool      `json:"ok"`
	Command  string    `json:"command,omitempty"`
	Context  string    `json:"context,omitempty"`
	Data     any       `json:"data,omitempty"`
	Warnings []Warning `json:"warnings,omitempty"`
	Next     []string  `json:"next,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// ErrorBody is the failure object inside a failure envelope.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Details any    `json:"details,omitempty"`
}

// Failure is the envelope printed for a failed command.
type Failure struct {
	Schema  string    `json:"schema"`
	OK      bool      `json:"ok"`
	Command string    `json:"command,omitempty"`
	Context string    `json:"context,omitempty"`
	Error   ErrorBody `json:"error"`
}

// Event is one NDJSON line on the RPC wire and for --stream.
type Event struct {
	Schema   string `json:"schema"`
	Type     string `json:"type"`
	Step     string `json:"step,omitempty"`
	State    string `json:"state,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Total    *int   `json:"total,omitempty"`
	Line     string `json:"line,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Pct      *int   `json:"pct,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	Envelope any    `json:"envelope,omitempty"`
	TS       string `json:"ts"`
}

// Result is a successful command payload before it is rendered.
type Result struct {
	Data     any
	Message  string
	Next     []string
	Warnings []Warning
}

// CLIError is a typed command error with a stable code and exit status.
type CLIError struct {
	Code     string
	Message  string
	Hint     string
	ExitCode int
	Details  any
	Cause    error
}

// CommandNext supplies CommandMeta.next when the output mode is agent.
var CommandNext func(cmd *cobra.Command) []string

// Error returns the human-readable message.
func (e *CLIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

// Unwrap returns the wrapped cause.
func (e *CLIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// WithDetails attaches a structured details object.
func (e *CLIError) WithDetails(details any) *CLIError {
	if e == nil {
		return nil
	}
	e.Details = details
	return e
}

// NewError creates a typed CLI error.
func NewError(code, message string, exitCode int, hint string) *CLIError {
	return &CLIError{
		Code:     code,
		Message:  message,
		Hint:     hint,
		ExitCode: exitCode,
	}
}

// WrapError creates a typed CLI error that preserves the cause.
func WrapError(code string, cause error, exitCode int, hint string) *CLIError {
	return &CLIError{
		Code:     code,
		Cause:    cause,
		ExitCode: exitCode,
		Hint:     hint,
	}
}

// ExitCode returns the process exit status for err.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) && cliErr != nil && cliErr.ExitCode != 0 {
		return cliErr.ExitCode
	}
	return classify(err).ExitCode
}

// outputFlagNames are the flags ParseOptions reads.
var outputFlagNames = []string{"output", "json", "jq", "quiet", "verbose", "stream"}

// RegisterPersistentFlags adds the shared output flags.
func RegisterPersistentFlags(cmd *cobra.Command) {
	addOutputFlags(cmd.PersistentFlags())
}

func addOutputFlags(flags *pflag.FlagSet) {
	flags.String("output", string(ModeHuman), "Output mode: human, json, agent, or markdown")
	flags.Bool("json", false, "Shorthand for --output json")
	flags.String("jq", "", "Filter the response envelope with a jq expression")
	flags.Bool("quiet", false, "Suppress non-essential human output")
	flags.Bool("verbose", false, "Include diagnostic detail")
	flags.Bool("stream", false, "Emit NDJSON events; the last line is a result")
}

// ParseOptions reads output flags from cmd.
func ParseOptions(cmd *cobra.Command) (Options, error) {
	if cmd == nil {
		return Options{Mode: ModeHuman}, nil
	}
	return optionsFromFlags(cmd.Flags())
}

// OptionsFromArgs reads output flags from raw arguments, ignoring every other
// flag. It covers errors raised before cobra reaches the output flags, such as
// an unknown command or an unknown flag earlier on the line.
func OptionsFromArgs(args []string) (Options, error) {
	flags := pflag.NewFlagSet("eru", pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.SetOutput(io.Discard)
	addOutputFlags(flags)
	if err := flags.Parse(args); err != nil {
		return Options{Mode: ModeHuman}, err
	}
	return optionsFromFlags(flags)
}

func optionsFromFlags(flags *pflag.FlagSet) (Options, error) {
	opts := Options{Mode: ModeHuman}
	outputName, outputSet := stringFlag(flags, "output")
	jsonMode, jsonSet := boolFlag(flags, "json")
	if jsonSet && jsonMode && outputSet && outputName != string(ModeJSON) {
		return opts, NewError(
			CodeOutputMode,
			"choose only one output mode",
			ExitUsage,
			"Use --output or --json, not both.",
		)
	}

	mode := outputName
	if mode == "" {
		mode = string(ModeHuman)
	}
	if jsonSet && jsonMode {
		mode = string(ModeJSON)
	}
	switch Mode(mode) {
	case ModeHuman, ModeJSON, ModeAgent, ModeMarkdown:
		opts.Mode = Mode(mode)
	default:
		return opts, NewError(
			CodeOutputMode,
			"invalid output mode "+mode,
			ExitUsage,
			"Use human, json, agent, or markdown.",
		)
	}

	opts.Quiet, _ = boolFlag(flags, "quiet")
	opts.JQ, _ = stringFlag(flags, "jq")
	opts.Verbose, _ = boolFlag(flags, "verbose")
	opts.Stream, _ = boolFlag(flags, "stream")
	return opts, nil
}

func outputFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range outputFlagNames {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			return true
		}
	}
	return false
}

// UsesStructuredOutput reports whether the invocation must not prompt.
func UsesStructuredOutput(opts Options) bool {
	return opts.Mode != ModeHuman || opts.JQ != "" || opts.Stream
}

// OK renders a successful result.
func OK(cmd *cobra.Command, data any, message string, next ...string) error {
	return Success(cmd, Result{Data: data, Message: message, Next: next})
}

// Success renders a successful result.
func Success(cmd *cobra.Command, result Result) error {
	opts, err := ParseOptions(cmd)
	if err != nil {
		return err
	}
	env := Envelope{
		Schema:   Schema,
		OK:       true,
		Command:  commandID(cmd),
		Context:  contextName(cmd),
		Data:     result.Data,
		Warnings: result.Warnings,
		Next:     result.Next,
	}
	if opts.Mode == ModeAgent {
		if CommandNext != nil {
			env.Next = mergeNext(env.Next, CommandNext(cmd))
		}
		env.Message = result.Message
	}
	return renderSuccess(cmd, opts, env, result.Message)
}

// Fail converts err into a failure envelope.
func Fail(cmd *cobra.Command, err error) Failure {
	cliErr := asCLI(err)
	if cliErr == nil {
		cliErr = classify(err)
	} else {
		copied := *cliErr
		cliErr = &copied
	}
	if cliErr.Code == "" {
		cliErr.Code = CodeError
	}
	if cliErr.ExitCode == 0 {
		cliErr.ExitCode = ExitGeneric
	}
	return Failure{
		Schema:  Schema,
		OK:      false,
		Command: commandID(cmd),
		Context: contextName(cmd),
		Error: ErrorBody{
			Code:    cliErr.Code,
			Message: cliErr.Error(),
			Hint:    cliErr.Hint,
			Details: cliErr.Details,
		},
	}
}

// RenderError writes a failure in the selected mode.
// Structured output goes to stdout. Human and markdown go to stderr.
func RenderError(cmd *cobra.Command, err error) error {
	if cmd == nil {
		_, werr := fmt.Fprintln(os.Stderr, err)
		return werr
	}
	opts, parseErr := ParseOptions(cmd)
	if parseErr == nil && !outputFlagsChanged(cmd) {
		// Cobra stops at the first unknown command or flag, so output flags
		// later on the line were never parsed. Read them from the raw arguments.
		raw, rawErr := OptionsFromArgs(os.Args[1:])
		var cliErr *CLIError
		switch {
		case rawErr == nil:
			opts = raw
		case errors.As(rawErr, &cliErr):
			parseErr = rawErr
		}
	}
	if parseErr != nil {
		err = parseErr
		opts = Options{}
	}
	failure := Fail(cmd, err)
	if opts.JQ != "" && parseErr == nil && !errors.Is(err, errJQFilter) {
		selected, jerr := applyJQ(failure, opts.JQ)
		if jerr != nil {
			return writeHumanError(cmd.ErrOrStderr(), Fail(cmd, jerr))
		}
		return writeJSON(cmd.OutOrStdout(), selected)
	}
	if opts.Stream && parseErr == nil {
		return writeEvent(cmd.OutOrStdout(), Event{Type: "result", Envelope: failure})
	}
	if opts.Mode == ModeJSON || opts.Mode == ModeAgent {
		return writeJSON(cmd.OutOrStdout(), failure)
	}
	if opts.Mode == ModeMarkdown {
		return writeMarkdownError(cmd.ErrOrStderr(), failure)
	}
	return writeHumanError(cmd.ErrOrStderr(), failure)
}

func renderSuccess(cmd *cobra.Command, opts Options, env Envelope, message string) error {
	out := io.Writer(os.Stdout)
	if cmd != nil {
		out = cmd.OutOrStdout()
	}
	if opts.JQ != "" {
		selected, err := applyJQ(env, opts.JQ)
		if err != nil {
			return err
		}
		return writeJSON(out, selected)
	}
	if opts.Stream {
		return writeEvent(out, Event{Type: "result", Envelope: env})
	}
	switch opts.Mode {
	case ModeJSON, ModeAgent:
		return writeJSON(out, env)
	case ModeMarkdown:
		return writeMarkdown(out, opts, message, env.Data, env.Next)
	default:
		return writeHuman(out, opts, message, unshownWarnings(cmd, env.Warnings), env.Next)
	}
}

func writeHuman(w io.Writer, opts Options, message string, warnings []Warning, next []string) error {
	if opts.Quiet {
		return nil
	}
	if message != "" {
		if _, err := fmt.Fprintln(w, message); err != nil {
			return err
		}
	}
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(w, "warning: %s\n", warning.Message); err != nil {
			return err
		}
	}
	if len(next) == 0 {
		return nil
	}
	if message != "" {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "Next steps:"); err != nil {
		return err
	}
	for _, step := range next {
		if _, err := fmt.Fprintf(w, "  %s\n", step); err != nil {
			return err
		}
	}
	return nil
}

type shownKey struct{}

// shownWarnings records the warning messages already printed while the
// command ran.
type shownWarnings struct {
	mu   sync.Mutex
	msgs map[string]bool
}

// TrackShownWarnings gives cmd a fresh record of warnings shown while it
// runs. The progress renderer calls it once per command.
func TrackShownWarnings(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.SetContext(context.WithValue(ctx, shownKey{}, &shownWarnings{msgs: map[string]bool{}}))
}

// MarkWarningShown records that msg was already printed to a human while
// cmd ran, so the human result does not print it a second time. JSON, agent
// and stream envelopes still carry every warning.
func MarkWarningShown(cmd *cobra.Command, msg string) {
	if set := shownSet(cmd); set != nil {
		set.mu.Lock()
		set.msgs[msg] = true
		set.mu.Unlock()
	}
}

func shownSet(cmd *cobra.Command) *shownWarnings {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	set, _ := cmd.Context().Value(shownKey{}).(*shownWarnings)
	return set
}

// unshownWarnings drops the warnings already printed while cmd ran.
func unshownWarnings(cmd *cobra.Command, warnings []Warning) []Warning {
	set := shownSet(cmd)
	if set == nil || len(warnings) == 0 {
		return warnings
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	out := make([]Warning, 0, len(warnings))
	for _, w := range warnings {
		if !set.msgs[w.Message] {
			out = append(out, w)
		}
	}
	return out
}

// RichError draws a human failure on a terminal. It returns false to fall
// back to the plain lines. The CLI sets it when stderr is a terminal.
var RichError func(w io.Writer, failure Failure) bool

func writeHumanError(w io.Writer, failure Failure) error {
	if RichError != nil && RichError(w, failure) {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Error: %s [%s]\n", failure.Error.Message, failure.Error.Code); err != nil {
		return err
	}
	if failure.Error.Hint != "" {
		if _, err := fmt.Fprintf(w, "Hint: %s\n", failure.Error.Hint); err != nil {
			return err
		}
	}
	return nil
}

func writeMarkdownError(w io.Writer, failure Failure) error {
	if _, err := fmt.Fprintf(w, "**Error:** %s\n\n`%s`\n", failure.Error.Message, failure.Error.Code); err != nil {
		return err
	}
	if failure.Error.Hint != "" {
		if _, err := fmt.Fprintf(w, "\n%s\n", failure.Error.Hint); err != nil {
			return err
		}
	}
	return nil
}

func writeMarkdown(w io.Writer, opts Options, message string, data any, next []string) error {
	if !opts.Quiet && message != "" {
		if _, err := fmt.Fprintln(w, message); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if err := writeMarkdownData(w, data); err != nil {
		return err
	}
	if opts.Quiet || len(next) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Next:"); err != nil {
		return err
	}
	for _, step := range next {
		if _, err := fmt.Fprintf(w, "- `%s`\n", step); err != nil {
			return err
		}
	}
	return nil
}

func writeMarkdownData(w io.Writer, data any) error {
	if data == nil {
		return nil
	}
	normalized, err := normalizeJSON(data)
	if err != nil {
		return err
	}
	switch value := normalized.(type) {
	case []any:
		return writeMarkdownSlice(w, value)
	case map[string]any:
		return writeMarkdownMap(w, value)
	default:
		return writeMarkdownJSON(w, value)
	}
}

func writeMarkdownSlice(w io.Writer, items []any) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "(empty)")
		return err
	}
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return writeMarkdownJSON(w, items)
		}
		rows = append(rows, obj)
	}
	return writeObjectTable(w, rows)
}

func writeMarkdownMap(w io.Writer, value map[string]any) error {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, err := fmt.Fprintln(w, "| Key | Value |"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "| --- | --- |"); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "| %s | %s |\n", escapeCell(key), escapeCell(fmt.Sprint(value[key]))); err != nil {
			return err
		}
	}
	return nil
}

func writeObjectTable(w io.Writer, rows []map[string]any) error {
	seen := map[string]bool{}
	keys := make([]string, 0)
	for _, row := range rows {
		for key := range row {
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(keys, " | ")); err != nil {
		return err
	}
	seps := make([]string, len(keys))
	for i := range seps {
		seps[i] = "---"
	}
	if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(seps, " | ")); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(keys))
		for i, key := range keys {
			cells[i] = escapeCell(fmt.Sprint(row[key]))
		}
		if _, err := fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | ")); err != nil {
			return err
		}
	}
	return nil
}

func writeMarkdownJSON(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "```json\n%s\n```\n", encoded)
	return err
}

func escapeCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func writeEvent(w io.Writer, event Event) error {
	event.Schema = EventSchema
	if event.TS == "" {
		event.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(event)
}

// errJQFilter marks errors raised by the --jq filter itself, so the failure
// is not filtered again.
var errJQFilter = errors.New("jq filter")

func jqError(message, hint string) *CLIError {
	return &CLIError{Code: CodeUsage, Message: message, Hint: hint, ExitCode: ExitUsage, Cause: errJQFilter}
}

func applyJQ(value any, expr string) (any, error) {
	normalized, err := normalizeJSON(value)
	if err != nil {
		return nil, jqError("response cannot be filtered with --jq", err.Error())
	}
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, jqError("invalid --jq expression", err.Error())
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, jqError("invalid --jq expression", err.Error())
	}
	iter := code.Run(normalized)
	results := make([]any, 0, 1)
	for {
		item, ok := iter.Next()
		if !ok {
			break
		}
		if jqErr, isErr := item.(error); isErr {
			// error/0 and error/1 carry a value that can be the whole envelope.
			// Keep it out of the hint.
			hint := jqErr.Error()
			var valueErr gojq.ValueError
			if errors.As(jqErr, &valueErr) {
				hint = "the filter called error"
			}
			return nil, jqError("jq filter failed", hint)
		}
		results = append(results, item)
	}
	switch len(results) {
	case 0:
		return nil, nil
	case 1:
		return results[0], nil
	default:
		return results, nil
	}
}

func normalizeJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func mergeNext(base, extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(base)+len(extra))
	for _, item := range append(append([]string{}, base...), extra...) {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func commandID(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	fields := strings.Fields(cmd.CommandPath())
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		return fields[0]
	}
	return strings.Join(fields[1:], " ")
}

func contextName(cmd *cobra.Command) string {
	if cmd == nil || cmd.Flags() == nil {
		return ""
	}
	flag := cmd.Flags().Lookup("context")
	if flag == nil {
		return ""
	}
	return strings.TrimSpace(flag.Value.String())
}

func stringFlag(flags *pflag.FlagSet, name string) (string, bool) {
	flag := flags.Lookup(name)
	if flag == nil {
		return "", false
	}
	return flag.Value.String(), flag.Changed
}

func boolFlag(flags *pflag.FlagSet, name string) (bool, bool) {
	flag := flags.Lookup(name)
	if flag == nil {
		return false, false
	}
	value, err := strconv.ParseBool(flag.Value.String())
	if err != nil {
		return false, flag.Changed
	}
	return value, flag.Changed
}

func asCLI(err error) *CLIError {
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		return cliErr
	}
	return nil
}

func classify(err error) *CLIError {
	if err == nil {
		return NewError(CodeError, "unknown error", ExitGeneric, "")
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "unknown command"),
		strings.Contains(low, "unknown flag"),
		strings.Contains(low, "unknown shorthand"),
		strings.Contains(low, "accepts "),
		strings.Contains(low, "arg(s)"),
		strings.Contains(low, "required flag"),
		strings.Contains(low, "invalid argument"),
		strings.Contains(low, "bad flag"),
		strings.Contains(low, "flag needs"):
		return NewError(CodeUsage, msg, ExitUsage, "")
	default:
		return NewError(CodeError, msg, ExitGeneric, "")
	}
}
