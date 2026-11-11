package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/diagnostics"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

// The CLI checks the same bounded shapes as the activity contract before it
// opens a session, so an invalid filter never reaches the node.
const activityCursorMax = 2048

var (
	activityIDPattern     = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	activityTokenPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	activityActionPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	activityCursorPattern = regexp.MustCompile(`^[A-Za-z0-9_.:+/=-]+$`)
)

// activityFilterFlags are the shared bounded filters. An empty sinceDefault
// leaves the window open; the store still applies its own byte, segment and
// retention bounds.
func activityFilterFlags(sinceDefault string, limitDefault int, limitUsage string) []fl {
	return []fl{
		flagString("since", sinceDefault, "Only events at or after this RFC3339 time or lookback duration (such as 24h)"),
		flagString("until", "", "Only events before this RFC3339 time or lookback duration"),
		flagString("operation", "", "Only this operation ID"),
		flagString("request", "", "Only this request ID"),
		flagString("job", "", "Only this job ID"),
		flagString("db", "", "Only this database"),
		flagString("action", "", "Only this action, such as backup.now or job.backup-full"),
		flagString("outcome", "", "Only started, succeeded, failed, refused, skipped or unknown"),
		flagInt("limit", limitDefault, limitUsage),
		flagString("cursor", "", "Continue from a page's next_cursor"),
	}
}

func activityCommands() cmdSpec {
	return cmdSpec{
		use: "activity",
		run: parentRun,
		meta: CommandMeta{
			Summary: "Search the node's bounded activity history",
			WhenToUse: []string{
				"Ask what happened to one operation without reading raw logs. Events carry typed actions, outcomes, correlation IDs, times and stable codes only.",
				"Only the context's master node is queried today; worker fan-out waits for fleet transport.",
			},
			Examples: []string{"vaire activity list --since 1h", "vaire activity show <operation-id>"},
			Remote:   true,
			ErrorCodes: []string{
				"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
				"node_version_mismatch", "validation_failed", "precondition_failed",
			},
		},
		children: []cmdSpec{
			{
				use:  "list",
				run:  runActivityList,
				args: cobra.NoArgs,
				flags: activityFilterFlags("", activity.DefaultLimit,
					"Events to return, 1-500 (default 100)"),
				meta: CommandMeta{
					Summary: "List typed activity events with stable cursors",
					WhenToUse: []string{
						"Filter the retained history by time, correlation IDs, database, action or outcome. Each page reports its coverage, whether it was truncated and a next cursor when more evidence may exist.",
						"An empty page with available true means nothing matched, not that the node is healthy. Available false means the history could not be read.",
					},
					Examples: []string{
						"vaire activity list --since 24h",
						"vaire activity list --db app --outcome failed --since 168h",
						"vaire activity list --operation <operation-id> --output json",
						"vaire activity list --cursor <next-cursor>",
					},
					Remote: true,
					ErrorCodes: []string{
						"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
						"node_version_mismatch", "validation_failed", "precondition_failed",
					},
					Next: []string{"vaire activity show <operation-id>"},
				},
			},
			{
				use:  "show [operation-id]",
				run:  runActivityShow,
				args: cobra.MaximumNArgs(1),
				flags: []fl{
					flagString("operation", "", "Select this operation ID instead of the positional argument"),
					flagString("request", "", "Select one request ID"),
					flagString("job", "", "Select one job ID"),
				},
				meta: CommandMeta{
					Summary: "Show one operation across its requests, steps and jobs",
					WhenToUse: []string{
						"Follow one operation ID returned by activity list. The page keeps occurrence times, per-node sequence, correlation IDs and every recorded event; a missing terminal event means the outcome is incomplete/unknown, not successful.",
						"Correlation IDs are exact matches; retries have their own request ID and remain part of the same operation.",
					},
					Examples: []string{
						"vaire activity show 01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
						"vaire activity show --request 01J9ZK3Q6V3W8Y2XH5N7T1C4BE",
						"vaire activity show --job 01J9ZK3Q6V3W8Y2XH5N7T1C4BF",
						"vaire activity show <operation-id> --output json",
					},
					Remote: true,
					ErrorCodes: []string{
						"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
						"node_version_mismatch", "validation_failed", "precondition_failed",
					},
					Next: []string{"vaire activity list --operation <operation-id>"},
				},
			},
			{
				use:  "health",
				run:  runActivityHealth,
				args: cobra.NoArgs,
				meta: CommandMeta{
					Summary: "Show activity store availability, size and loss counters",
					WhenToUse: []string{
						"Check whether evidence is available at all, whether files stay owner-only, and whether rotation, malformed data or append failures dropped events. The answer is typed metadata, not free-form log text.",
					},
					Examples: []string{"vaire activity health", "vaire activity health --output json"},
					Remote:   true,
					ErrorCodes: []string{
						"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
						"node_version_mismatch", "precondition_failed", "validation_failed",
					},
					Next: []string{"vaire activity list --since 1h"},
				},
			},
		},
	}
}

func runActivityList(cmd *cobra.Command, _ []string) error {
	q, err := activityQueryFromFlags(cmd)
	if err != nil {
		return err
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "activity.list", q, quietCallOptions())
	if err != nil {
		return err
	}
	page, err := decodeNodeData[activity.Page](reply, "activity page")
	if err != nil {
		return err
	}
	page = redactActivityPage(page)
	if err := diagnostics.ValidatePage(page); err != nil {
		return err
	}
	warnings := append([]output.Warning{}, s.warnings...)
	warnings = append(warnings, reply.Warnings...)
	warnings = append(warnings, activityCoverageWarnings(page.Coverage)...)
	return output.Success(cmd, output.Result{
		Data:     page,
		Message:  activityPageMessage(page),
		Warnings: warnings,
		Next:     activityNext(page, q, s.ctx.Name),
	})
}

// activityShowQuery picks exactly one correlation selector. The positional
// argument is the operation ID; --operation is its flag form, and --request or
// --job select one attempt or job for the same read-only view.
func activityShowQuery(cmd *cobra.Command, args []string) (activity.Query, error) {
	q := activity.Query{Limit: activity.MaxLimit}
	if len(args) > 1 {
		return q, activityUsageError("operation", "too_many", "only one correlation ID may be given",
			"Use vaire activity show <operation-id>, or --request or --job.")
	}
	selectors := 0
	if len(args) == 1 {
		q.OperationID = strings.ToUpper(strings.TrimSpace(args[0]))
		selectors++
	}
	if operation := strings.ToUpper(flagStringIf(cmd, "operation")); operation != "" {
		q.OperationID = operation
		selectors++
	}
	if request := strings.ToUpper(flagStringIf(cmd, "request")); request != "" {
		q.RequestID = request
		selectors++
	}
	if job := strings.ToUpper(flagStringIf(cmd, "job")); job != "" {
		q.JobID = job
		selectors++
	}
	if selectors == 0 {
		return q, activityUsageError("operation", "required", "an operation, request or job ID is required",
			"Use vaire activity show <operation-id>, or --request or --job.")
	}
	if selectors > 1 {
		return q, activityUsageError("operation", "too_many", "choose exactly one operation, request or job ID", "")
	}
	for _, selector := range []struct {
		name  string
		value string
	}{
		{"operation", q.OperationID},
		{"request", q.RequestID},
		{"job", q.JobID},
	} {
		if selector.value != "" && !activityIDPattern.MatchString(selector.value) {
			return q, activityUsageError(selector.name, "not_allowed",
				"the "+selector.name+" ID is not a ULID", "Use an ID from vaire activity list.")
		}
	}
	if err := activity.ValidateQuery(q); err != nil {
		return q, activityUsageError("query", "invalid", "the activity query is not valid", "")
	}
	return q, nil
}

func runActivityShow(cmd *cobra.Command, args []string) error {
	q, err := activityShowQuery(cmd, args)
	if err != nil {
		return err
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "activity.show", q, quietCallOptions())
	if err != nil {
		return err
	}
	page, err := decodeNodeData[activity.Page](reply, "activity page")
	if err != nil {
		return err
	}
	page = redactActivityPage(page)
	if err := diagnostics.ValidatePage(page); err != nil {
		return err
	}
	warnings := append([]output.Warning{}, s.warnings...)
	warnings = append(warnings, reply.Warnings...)
	warnings = append(warnings, activityCoverageWarnings(page.Coverage)...)
	return output.Success(cmd, output.Result{
		Data:     page,
		Message:  activityPageMessage(page),
		Warnings: warnings,
		Next:     activityNext(page, q, s.ctx.Name),
	})
}

func runActivityHealth(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "activity.health", nil, quietCallOptions())
	if err != nil {
		return err
	}
	health, err := decodeNodeData[activity.Health](reply, "activity health")
	if err != nil {
		return err
	}
	health = redactActivityHealth(health)
	if err := diagnostics.ValidateHealth(health); err != nil {
		return err
	}
	warnings := append([]output.Warning{}, s.warnings...)
	warnings = append(warnings, reply.Warnings...)
	return output.Success(cmd, output.Result{
		Data:     health,
		Message:  activityHealthMessage(health),
		Warnings: warnings,
	})
}

// quietCallOptions keeps a read-only activity call from printing node log
// events or free-form detail. Node warnings still arrive in the reply.
func quietCallOptions() remote.CallOptions {
	return remote.CallOptions{OnEvent: func(output.Event) {}}
}

func activityQueryFromFlags(cmd *cobra.Command) (activity.Query, error) {
	q := activity.Query{
		Since:       flagStringIf(cmd, "since"),
		Until:       flagStringIf(cmd, "until"),
		OperationID: strings.ToUpper(flagStringIf(cmd, "operation")),
		RequestID:   strings.ToUpper(flagStringIf(cmd, "request")),
		JobID:       strings.ToUpper(flagStringIf(cmd, "job")),
		Database:    flagStringIf(cmd, "db"),
		Action:      flagStringIf(cmd, "action"),
		Outcome:     flagStringIf(cmd, "outcome"),
		Cursor:      flagStringIf(cmd, "cursor"),
	}
	if flag := cmd.Flags().Lookup("limit"); flag != nil {
		limit, err := cmd.Flags().GetInt("limit")
		if err != nil {
			return q, activityUsageError("limit", "invalid", "the event limit is not a number", "")
		}
		q.Limit = limit
	}
	if err := validateActivityQuery(q); err != nil {
		return q, err
	}
	return q, nil
}

func validateActivityQuery(q activity.Query) error {
	if q.Limit < 1 || q.Limit > activity.MaxLimit {
		return activityUsageError("limit", "out_of_range",
			fmt.Sprintf("the event limit must be between 1 and %d", activity.MaxLimit), "")
	}
	if q.Outcome != "" && !containsString(activity.Outcomes, q.Outcome) {
		return activityUsageError("outcome", "not_allowed", "the outcome is not a known activity outcome",
			"Use started, succeeded, failed, refused, skipped or unknown.")
	}
	for _, filter := range []struct {
		name    string
		value   string
		pattern *regexp.Regexp
	}{
		{"operation", q.OperationID, activityIDPattern},
		{"request", q.RequestID, activityIDPattern},
		{"job", q.JobID, activityIDPattern},
		{"db", q.Database, activityTokenPattern},
		{"action", q.Action, activityActionPattern},
	} {
		if filter.value != "" && !filter.pattern.MatchString(filter.value) {
			return activityUsageError(filter.name, "not_allowed",
				"the "+filter.name+" filter is not a bounded identifier", "")
		}
	}
	if q.Cursor != "" && (len(q.Cursor) > activityCursorMax || !activityCursorPattern.MatchString(q.Cursor)) {
		return activityUsageError("cursor", "not_allowed", "the cursor is not a bounded page cursor", "")
	}
	if err := validateActivityWindow(q.Since, q.Until); err != nil {
		return err
	}
	// The store owns cursor decoding and the mixed absolute/lookback window
	// comparison. Reuse its validator after the CLI's user-friendly checks so
	// continuation never reaches the node with a cursor it cannot honor.
	if err := activity.ValidateQuery(q); err != nil {
		return activityUsageError("query", "invalid", "the activity query is not valid",
			"Check the filters, limit and cursor; a cursor only works with the query that created it.")
	}
	return nil
}

func validateActivityWindow(since, until string) error {
	if len(since) > 64 {
		return activityUsageError("since", "too_long", "the since value is too long",
			"Use an RFC3339 time or a lookback duration such as 24h.")
	}
	if len(until) > 64 {
		return activityUsageError("until", "too_long", "the until value is too long",
			"Use an RFC3339 time or a lookback duration such as 24h.")
	}
	sinceAt, sinceTime := activityWindowValue(since)
	if since != "" && sinceAt == nil && !sinceTime {
		return activityUsageError("since", "not_a_time",
			"the since value is neither RFC3339 nor a positive duration", "Use 24h or 2026-10-26T10:00:00Z.")
	}
	untilAt, untilTime := activityWindowValue(until)
	if until != "" && untilAt == nil && !untilTime {
		return activityUsageError("until", "not_a_time",
			"the until value is neither RFC3339 nor a positive duration", "Use 24h or 2026-10-26T10:00:00Z.")
	}
	if sinceAt != nil && untilAt != nil && !sinceAt.Before(*untilAt) {
		return activityUsageError("until", "not_after_since", "the until time must be after the since time", "")
	}
	if sinceTime && untilTime {
		sinceLookback, _ := time.ParseDuration(since)
		untilLookback, _ := time.ParseDuration(until)
		// Since resolves to now-lookback; it must land before Until.
		if sinceLookback <= untilLookback {
			return activityUsageError("until", "not_after_since", "the until lookback must be shorter than the since lookback", "")
		}
	}
	return nil
}

// activityWindowValue reports an absolute time, or that value was a positive
// lookback duration the store resolves itself.
func activityWindowValue(value string) (*time.Time, bool) {
	if value == "" {
		return nil, false
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return &at, false
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return nil, true
	}
	return nil, false
}

func activityUsageError(field, reason, message, hint string) error {
	return output.NewError(output.CodeUsage, message, output.ExitUsage, hint).
		WithDetails(map[string]any{"field": field, "reason": reason})
}

func flagStringIf(cmd *cobra.Command, name string) string {
	if cmd.Flags().Lookup(name) == nil {
		return ""
	}
	return stringFlag(cmd, name)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// decodeNodeData decodes a typed node reply and rejects unknown fields or a
// second JSON value. Only the typed structure is kept; a hostile reply cannot
// smuggle arbitrary keys into output or a bundle.
func decodeNodeData[T any](reply remote.Reply, what string) (T, error) {
	var out T
	if len(reply.Data) > int(diagnostics.MaxBundleBytes) {
		return out, output.NewError(output.CodeValidation, "the node's "+what+" is larger than the safety limit", output.ExitConfig,
			"Query a smaller activity window.").
			WithDetails(map[string]any{"reason": "reply_too_large"})
	}
	if len(bytes.TrimSpace(reply.Data)) == 0 {
		return out, output.NewError(output.CodeValidation, "the node returned no "+what, output.ExitConfig,
			"Upgrade the node with vaire node upgrade, then retry.").
			WithDetails(map[string]any{"reason": "empty_reply"})
	}
	dec := json.NewDecoder(bytes.NewReader(reply.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, output.NewError(output.CodeValidation, "the node's "+what+" does not match the typed contract", output.ExitConfig,
			"Upgrade the node with vaire node upgrade, then retry.").
			WithDetails(map[string]any{"reason": "decode_failed"})
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return out, output.NewError(output.CodeValidation, "the node's "+what+" has trailing data", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "trailing_data"})
	}
	return out, nil
}

func activityPageMessage(page activity.Page) string {
	lines := make([]string, 0, len(page.Events)+2)
	switch {
	case !page.Coverage.Available:
		lines = append(lines, "activity history is unavailable on "+activityNode(page.Coverage)+
			"; this page is missing evidence, not a clean result")
	case len(page.Events) == 0:
		if page.Coverage.Partial {
			lines = append(lines, "no matching activity events in the readable evidence; the requested history is incomplete")
		} else {
			lines = append(lines, "no matching activity events in the retained history")
		}
	default:
		events := append([]activity.Event(nil), page.Events...)
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].OccurredAt != events[j].OccurredAt {
				return events[i].OccurredAt < events[j].OccurredAt
			}
			return events[i].Sequence < events[j].Sequence
		})
		for _, ev := range events {
			lines = append(lines, activityEventLine(ev))
		}
	}
	lines = append(lines, activityCoverageLine(page.Coverage))
	if page.NextCursor != "" {
		lines = append(lines, "more evidence may be available; next cursor "+activityCursorCell(page.NextCursor))
	}
	return strings.Join(lines, "\n")
}

func activityEventLine(ev activity.Event) string {
	parts := []string{
		activityCell(ev.OccurredAt),
		activityCell(ev.Action),
		activityCell(ev.Outcome),
	}
	if ev.Database != "" {
		parts = append(parts, "db="+activityCell(ev.Database))
	}
	if ev.Step != "" {
		parts = append(parts, "step="+activityCell(ev.Step))
	}
	if ev.OperationID != "" {
		parts = append(parts, "operation="+activityCell(ev.OperationID))
	}
	if ev.RequestID != "" {
		parts = append(parts, "request="+activityCell(ev.RequestID))
	}
	if ev.JobID != "" {
		parts = append(parts, "job="+activityCell(ev.JobID))
	}
	if ev.ErrorCode != "" {
		parts = append(parts, "error="+activityCell(ev.ErrorCode))
	}
	if ev.ReasonCode != "" {
		parts = append(parts, "reason="+activityCell(ev.ReasonCode))
	}
	if ev.DurationMS > 0 {
		parts = append(parts, fmt.Sprintf("%dms", ev.DurationMS))
	}
	if ev.ActorKind != "" {
		actor := "actor=" + activityCell(ev.ActorKind)
		if ev.ActorLabel != "" {
			actor += ":" + activityCell(ev.ActorLabel)
		}
		if ev.ActorKind == activity.ActorOperator && !ev.ActorVerified {
			actor += " (claimed)"
		}
		parts = append(parts, actor)
	}
	return strings.Join(parts, "  ")
}

func activityCoverageLine(c activity.Coverage) string {
	availability := "available"
	if !c.Available {
		availability = "unavailable"
	}
	parts := []string{
		availability,
		"node=" + activityNode(c),
		fmt.Sprintf("%d returned, %d scanned", c.EventsReturned, c.EventsScanned),
		fmt.Sprintf("%d segments, %d bytes", c.SegmentsScanned, c.BytesScanned),
	}
	if c.FirstRetainedAt != "" || c.LastRetainedAt != "" {
		parts = append(parts, "retained "+activityCell(c.FirstRetainedAt)+" to "+activityCell(c.LastRetainedAt))
	}
	flags := make([]string, 0, 5)
	if c.Partial {
		flags = append(flags, "partial")
	}
	if c.Truncated {
		flags = append(flags, "truncated")
	}
	if c.LimitReached {
		flags = append(flags, "limit-reached")
	}
	if c.CursorExpired {
		flags = append(flags, "cursor-expired")
	}
	if c.LossDetected {
		flags = append(flags, "loss-detected")
	}
	if len(flags) > 0 {
		parts = append(parts, "coverage="+strings.Join(flags, ","))
	}
	if c.MalformedSegments > 0 || c.MalformedEvents > 0 || c.DroppedEvents > 0 || c.SequenceGaps > 0 {
		parts = append(parts, fmt.Sprintf("malformed segments=%d events=%d dropped=%d sequence-gaps=%d",
			c.MalformedSegments, c.MalformedEvents, c.DroppedEvents, c.SequenceGaps))
	}
	return "coverage: " + strings.Join(parts, "; ")
}

func activityNode(c activity.Coverage) string {
	if c.Node == "" {
		return "-"
	}
	return activityCell(c.Node)
}

// activityNext keeps the filters a cursor is bound to. The store refuses a
// cursor presented with different filters, so the continuation command must
// repeat them.
func activityNext(page activity.Page, q activity.Query, contextName string) []string {
	if page.NextCursor == "" {
		return nil
	}
	parts := []string{"vaire"}
	if contextName != "" {
		parts = append(parts, "--context", activityCell(contextName))
	}
	parts = append(parts, "activity", "list")
	for _, filter := range []struct {
		name  string
		value string
	}{
		{"since", q.Since},
		{"until", q.Until},
		{"operation", q.OperationID},
		{"request", q.RequestID},
		{"job", q.JobID},
		{"db", q.Database},
		{"action", q.Action},
		{"outcome", q.Outcome},
	} {
		if filter.value != "" {
			parts = append(parts, "--"+filter.name, activityCell(filter.value))
		}
	}
	if q.Limit > 0 {
		parts = append(parts, "--limit", fmt.Sprintf("%d", q.Limit))
	}
	parts = append(parts, "--cursor", activityCursorCell(page.NextCursor))
	return []string{strings.Join(parts, " ")}
}

func activityCoverageWarnings(c activity.Coverage) []output.Warning {
	warnings := make([]output.Warning, 0, 5)
	if !c.Available {
		warnings = append(warnings, output.Warning{
			Code:    "activity_unavailable",
			Message: "the node could not read its activity history; missing evidence is not a clean result",
		})
	}
	if c.Partial {
		warnings = append(warnings, output.Warning{
			Code:    "activity_partial",
			Message: "the requested activity evidence is incomplete; read the coverage fields",
		})
	}
	if c.Truncated {
		warnings = append(warnings, output.Warning{
			Code:    "activity_truncated",
			Message: "the activity scan stopped at a bound before the requested window was exhausted",
		})
	}
	if c.CursorExpired {
		warnings = append(warnings, output.Warning{
			Code:    "activity_cursor_expired",
			Message: "the activity cursor pointed into evidence removed by rotation or retention",
		})
	}
	if c.LossDetected {
		warnings = append(warnings, output.Warning{
			Code:    "activity_loss_detected",
			Message: "the activity store reports dropped, malformed or sequence-gap evidence",
		})
	}
	return warnings
}

func activityHealthMessage(h activity.Health) string {
	node := h.Node
	if node == "" {
		node = "-"
	}
	lines := []string{
		"activity store on " + activityCell(node) + " at " + activityCell(h.Dir),
		fmt.Sprintf("exists=%t is_dir=%t readable=%t writable=%t owner_only=%t",
			h.Exists, h.IsDir, h.Readable, h.Writable, h.OwnerOnly),
		fmt.Sprintf("segments=%d used_bytes=%d max_bytes=%d segment_bytes=%d retention_s=%d over_budget=%t",
			h.Segments, h.UsedBytes, h.MaxBytes, h.SegmentBytes, h.RetentionS, h.OverBudget),
	}
	if h.DirMode != "" || h.SegmentMode != "" {
		lines = append(lines, "dir_mode="+activityCell(h.DirMode)+" segment_mode="+activityCell(h.SegmentMode)+
			fmt.Sprintf(" non_owner_only_segments=%d", h.NonOwnerOnlySegments))
	}
	if h.FirstRetainedAt != "" || h.LastRetainedAt != "" || h.LastWriteAt != "" {
		lines = append(lines, "retained "+activityCell(h.FirstRetainedAt)+" to "+activityCell(h.LastRetainedAt)+
			" last_write="+activityCell(h.LastWriteAt))
	}
	lines = append(lines, fmt.Sprintf("malformed_segments=%d malformed_events=%d dropped_events=%d sequence_gaps=%d loss_detected=%t",
		h.MalformedSegments, h.MalformedEvents, h.DroppedEvents, h.SequenceGaps, h.LossDetected))
	return strings.Join(lines, "\n")
}

// activityCursorCell keeps an opaque cursor intact for a copy-pasteable
// continuation command. Cursors are much longer than ordinary labels, so the
// 96-byte display bound does not apply; control characters are still removed.
func activityCursorCell(value string) string {
	if value == "" {
		return "-"
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 2048 {
			b.WriteByte('?')
			break
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

// activityCell is a single-line, bounded rendering for human output. It keeps
// a hostile node reply from injecting terminal control sequences or new
// lines; JSON output keeps the typed value and its normal escaping.
func activityCell(value string) string {
	if value == "" {
		return "-"
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 96 {
			b.WriteByte('?')
			break
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}
