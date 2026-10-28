package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/diagnostics"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/spf13/cobra"
)

// diagnosticsCollectResult is the structured result of a successful collect.
// The bundle is the exact document written to disk; the path is not part of
// the bundle contract.
type diagnosticsCollectResult struct {
	Path   string             `json:"path"`
	Query  activity.Query     `json:"query"`
	Bundle diagnostics.Bundle `json:"bundle"`
}

// diagnosticsCollectPlan is the dry-run answer: the target and the bounded
// query, with no node contact and no file.
type diagnosticsCollectPlan struct {
	Path   string         `json:"path"`
	Query  activity.Query `json:"query"`
	DryRun bool           `json:"dry_run"`
}

func diagnosticsCommands() cmdSpec {
	return cmdSpec{
		use: "diagnostics",
		run: parentRun,
		meta: CommandMeta{
			Summary: "Collect and inspect portable incident evidence",
			WhenToUse: []string{
				"Share typed activity and allowlisted metadata with an agent without handing over an operator context, server credentials or raw diagnostic logs.",
				"Collection uses the existing SSH transport and reads one bounded activity page; inspection is fully offline.",
			},
			Examples: []string{
				"vaire diagnostics collect --since 24h --out incident.json",
				"vaire diagnostics inspect incident.json",
			},
			Remote: true,
			ErrorCodes: []string{
				"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
				"node_version_mismatch", "validation_failed", "precondition_failed",
			},
		},
		children: []cmdSpec{
			{
				use:  "collect",
				run:  runDiagnosticsCollect,
				args: cobra.NoArgs,
				flags: append(activityFilterFlags("24h", activity.MaxLimit,
					"Events to include, 1-500 (default 500)"), flagString("out", "",
					"Write the incident bundle here (required; never overwritten)")),
				meta: CommandMeta{
					Summary: "Write one bounded incident bundle from the context's master",
					WhenToUse: []string{
						"Capture typed activity events and fixed metadata for one incident window. The bundle carries the page's coverage, truncation and next cursor, so incomplete evidence stays explicit.",
						"Only the context's master node is queried today. The file is owner-only, at most 8 MiB, and never overwrites an existing path.",
					},
					Examples: []string{
						"vaire diagnostics collect --since 24h --out incident.json",
						"vaire diagnostics collect --since 2h --db app --out app-incident.json",
						"vaire diagnostics collect --operation <operation-id> --out operation.json",
						"vaire diagnostics collect --since 24h --out incident.json --dry-run",
					},
					Mutating:       true,
					SupportsDryRun: true,
					Remote:         true,
					ErrorCodes: []string{
						"context_not_found", "remote_unreachable", "transport_failed", "host_key_mismatch",
						"node_version_mismatch", "validation_failed", "precondition_failed",
					},
					Next: []string{"vaire diagnostics inspect <bundle-file>"},
				},
			},
			{
				use:  "inspect <bundle-file>",
				run:  runDiagnosticsInspect,
				args: cobra.ExactArgs(1),
				meta: CommandMeta{
					Summary: "Read and validate an incident bundle offline",
					WhenToUse: []string{
						"Work from a collected bundle without a vaire context, server credentials or network access. The reader rejects unknown fields at every level, trailing JSON, symlinks, oversized files and bundles other users can read.",
						"JSON output is the parsed, validated bundle itself; no raw JSON is passed through.",
					},
					Examples: []string{
						"vaire diagnostics inspect incident.json",
						"vaire diagnostics inspect incident.json --output json",
					},
					ErrorCodes: []string{
						"bundle_corrupt", "bundle_not_found", "validation_failed", "precondition_failed",
					},
				},
			},
		},
	}
}

func runDiagnosticsCollect(cmd *cobra.Command, _ []string) error {
	q, err := activityQueryFromFlags(cmd)
	if err != nil {
		return err
	}
	// A registered secret in a filter must never reach a bundle, a dry-run
	// plan or a result. Refuse before contacting the node.
	safeQuery, err := safeActivityQuery(q)
	if err != nil {
		return err
	}
	out := stringFlag(cmd, "out")
	if out == "" {
		return activityUsageError("out", "required", "an output file is required",
			"Pass --out <file>; the bundle is always written to a local file.")
	}
	// Validate the target and every filter before opening a tunnel or session.
	if err := diagnostics.CheckWritePath(out); err != nil {
		return err
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{
			Data: diagnosticsCollectPlan{Path: out, Query: safeQuery, DryRun: true},
			Message: "dry run: no node was contacted and no bundle was written; would write " +
				activityCell(out),
		})
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "activity.list", q, quietCallOptions())
	if err != nil {
		// A remote failure never produces a success bundle.
		return err
	}
	page, err := decodeNodeData[activity.Page](reply, "activity page")
	if err != nil {
		return err
	}
	// Opening the session registered its tunnel key; re-check before any
	// bundle or result is produced.
	if safeQuery, err = safeActivityQuery(q); err != nil {
		return err
	}
	server := s.ctx.Server
	if server == "" {
		server = s.ctx.Name
	}
	node := s.ctx.Name
	if spec, ok := s.ctx.Master(); ok {
		node = nodeLabel(s.ctx, spec)
	}
	collectedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	bundle := newCollectBundle(server, node, collectedAt, safeQuery, page)
	if err := diagnostics.Write(out, bundle); err != nil {
		return err
	}
	warnings := append([]output.Warning{}, s.warnings...)
	warnings = append(warnings, reply.Warnings...)
	warnings = append(warnings, activityCoverageWarnings(bundle.Activity.Coverage)...)
	if bundle.Activity.NextCursor != "" {
		warnings = append(warnings, output.Warning{
			Code:    "activity_continues",
			Message: "more matching activity may be available; collect the next page from context " + s.ctx.Name + " with --cursor, the same filters and limit, and a new --out",
		})
	}
	message := fmt.Sprintf("wrote incident bundle %s: %d events from %s\n%s",
		activityCell(out), len(bundle.Activity.Events), activityNode(bundle.Activity.Coverage),
		activityCoverageLine(bundle.Activity.Coverage))
	if bundle.Activity.NextCursor != "" {
		message += "\nmore evidence may be available; next cursor " + activityCursorCell(bundle.Activity.NextCursor)
	}
	return output.Success(cmd, output.Result{
		Data:     diagnosticsCollectResult{Path: out, Query: safeQuery, Bundle: bundle},
		Message:  message,
		Warnings: warnings,
		Next:     []string{diagnosticsInspectCommand(out)},
	})
}

func runDiagnosticsInspect(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return activityUsageError("bundle", "required", "an incident bundle file is required",
			"Use vaire diagnostics inspect <bundle-file>.")
	}
	// Offline by construction: no context resolution, tunnel or session.
	bundle, err := diagnostics.Read(args[0])
	if err != nil {
		return err
	}
	warnings := activityCoverageWarnings(bundle.Activity.Coverage)
	return output.Success(cmd, output.Result{
		Data:     bundle,
		Message:  diagnosticsBundleMessage(args[0], bundle),
		Warnings: warnings,
	})
}

// diagnosticsInspectCommand renders the next command with the path quoted for
// a POSIX shell, so spaces and quotes in a bundle name stay copy-pasteable.
func diagnosticsInspectCommand(path string) string {
	return "vaire diagnostics inspect " + "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// newCollectBundle is the single place a collect result becomes a bundle: the
// query is an actual typed activity.Query (redacted before validation), and no
// resolved window or arbitrary filter map is invented.
func newCollectBundle(server, node, collectedAt string, q activity.Query, page activity.Page) diagnostics.Bundle {
	safeQuery := redactActivityQuery(q)
	return diagnostics.Bundle{
		Schema:    diagnostics.Schema,
		CreatedAt: collectedAt,
		Metadata: diagnostics.Metadata{
			Server:        secrets.Redact(server),
			Node:          secrets.Redact(node),
			NodeVersion:   secrets.Redact(activityNodeMetadataVersion()),
			ClientVersion: secrets.Redact(activityClientVersion()),
			CollectedAt:   collectedAt,
		},
		Query:    &safeQuery,
		Activity: redactActivityPage(page),
	}
}

func safeActivityQuery(q activity.Query) (activity.Query, error) {
	safe := redactActivityQuery(q)
	if safe != q {
		return safe, output.NewError(output.CodeValidation, "the activity query contains a registered secret", output.ExitConfig,
			"Choose filters that do not contain registered secrets.").
			WithDetails(map[string]any{"reason": "query_secret"})
	}
	return safe, nil
}

func redactActivityQuery(q activity.Query) activity.Query {
	q.Since = secrets.Redact(q.Since)
	q.Until = secrets.Redact(q.Until)
	q.OperationID = secrets.Redact(q.OperationID)
	q.RequestID = secrets.Redact(q.RequestID)
	q.JobID = secrets.Redact(q.JobID)
	q.Database = secrets.Redact(q.Database)
	q.Action = secrets.Redact(q.Action)
	q.Outcome = secrets.Redact(q.Outcome)
	q.Cursor = secrets.Redact(q.Cursor)
	return q
}

// activityClientVersion is the exact build string this operator runs,
// including the commit when one was stamped. It matches the node emitter's
// version shape.
func activityClientVersion() string {
	return activityBuildVersion(buildinfo.Version, buildinfo.Commit)
}

// activityNodeMetadataVersion names the node build. The startup handshake
// requires the same version and channel, and for dev builds the same commit;
// for a released build the commit is not checked, so node metadata carries the
// checked version instead of pretending the operator's commit is the node's.
func activityNodeMetadataVersion() string {
	if buildinfo.Channel == "dev" {
		return activityBuildVersion(buildinfo.Version, buildinfo.Commit)
	}
	return activityBuildVersion(buildinfo.Version, "")
}

func activityBuildVersion(version, commit string) string {
	if commit != "" && commit != "none" {
		version += "+" + commit
	}
	if len(version) > 64 {
		version = version[:64]
	}
	return version
}

func redactActivityHealth(h activity.Health) activity.Health {
	h.Node = secrets.Redact(h.Node)
	h.Dir = secrets.Redact(h.Dir)
	h.DirMode = secrets.Redact(h.DirMode)
	h.SegmentMode = secrets.Redact(h.SegmentMode)
	h.FirstRetainedAt = secrets.Redact(h.FirstRetainedAt)
	h.LastRetainedAt = secrets.Redact(h.LastRetainedAt)
	h.LastWriteAt = secrets.Redact(h.LastWriteAt)
	return h
}

func diagnosticsBundleMessage(path string, b diagnostics.Bundle) string {
	lines := []string{
		"incident bundle " + activityCell(path) + " created " + activityCell(b.CreatedAt),
		"server=" + activityCell(b.Metadata.Server) + " node=" + activityCell(b.Metadata.Node) +
			" node_version=" + activityCell(b.Metadata.NodeVersion) +
			" client_version=" + activityCell(b.Metadata.ClientVersion) +
			" collected_at=" + activityCell(b.Metadata.CollectedAt),
	}
	if b.Query != nil {
		lines = append(lines, "query: "+diagnosticsQuerySummary(*b.Query))
	}
	lines = append(lines, strings.Split(activityPageMessage(b.Activity), "\n")...)
	return strings.Join(lines, "\n")
}

// diagnosticsQuerySummary renders the stored selection without inventing a
// resolved node window. A cursor is shown as set, not copied into a long line.
func diagnosticsQuerySummary(q activity.Query) string {
	parts := make([]string, 0, 9)
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
			parts = append(parts, filter.name+"="+activityCell(filter.value))
		}
	}
	if q.Limit > 0 {
		parts = append(parts, fmt.Sprintf("limit=%d", q.Limit))
	}
	if q.Cursor != "" {
		parts = append(parts, "cursor=set")
	}
	if len(parts) == 0 {
		return "all retained activity"
	}
	return strings.Join(parts, " ")
}

// redactActivityPage applies the registered-value redaction registry before
// validation. A value that becomes a placeholder no longer matches the
// bounded shapes, so collection fails safely instead of writing a secret.
func redactActivityPage(page activity.Page) activity.Page {
	if page.Events == nil {
		page.Events = []activity.Event{}
	}
	page.Schema = secrets.Redact(page.Schema)
	page.NextCursor = secrets.Redact(page.NextCursor)
	c := page.Coverage
	c.Node = secrets.Redact(c.Node)
	c.FirstRetainedAt = secrets.Redact(c.FirstRetainedAt)
	c.LastRetainedAt = secrets.Redact(c.LastRetainedAt)
	c.FirstReturnedAt = secrets.Redact(c.FirstReturnedAt)
	c.LastReturnedAt = secrets.Redact(c.LastReturnedAt)
	page.Coverage = c
	for i := range page.Events {
		ev := page.Events[i]
		ev.Schema = secrets.Redact(ev.Schema)
		ev.EventID = secrets.Redact(ev.EventID)
		ev.OccurredAt = secrets.Redact(ev.OccurredAt)
		ev.Server = secrets.Redact(ev.Server)
		ev.Node = secrets.Redact(ev.Node)
		ev.NodeInstanceID = secrets.Redact(ev.NodeInstanceID)
		ev.BootID = secrets.Redact(ev.BootID)
		ev.OperationID = secrets.Redact(ev.OperationID)
		ev.RequestID = secrets.Redact(ev.RequestID)
		ev.ParentRequestID = secrets.Redact(ev.ParentRequestID)
		ev.JobID = secrets.Redact(ev.JobID)
		ev.Source = secrets.Redact(ev.Source)
		ev.Action = secrets.Redact(ev.Action)
		ev.Step = secrets.Redact(ev.Step)
		ev.Database = secrets.Redact(ev.Database)
		ev.Outcome = secrets.Redact(ev.Outcome)
		ev.ErrorCode = secrets.Redact(ev.ErrorCode)
		ev.ReasonCode = secrets.Redact(ev.ReasonCode)
		ev.ActorKind = secrets.Redact(ev.ActorKind)
		ev.ActorLabel = secrets.Redact(ev.ActorLabel)
		ev.NodeVersion = secrets.Redact(ev.NodeVersion)
		page.Events[i] = ev
	}
	return page
}
