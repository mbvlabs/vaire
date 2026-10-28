package node

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

// The activity methods are read-only history queries. They take no run log
// and no node lock: asking what happened must not itself become an operation,
// a lock wait or a recursive history entry. They also never create the store;
// a missing directory is reported through Health or Coverage, not by making
// one.

// activityShowDefaultLimit keeps one shown operation bounded even when the
// caller asks for no explicit page size.
const activityShowDefaultLimit = 150

var activityQueryActionPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
var activityCursorPattern = regexp.MustCompile(`^[A-Za-z0-9_.:+/=-]*$`)

func handleActivityList(ctx context.Context, env *Env) (Result, error) {
	var q activity.Query
	if err := decodeParams(env.Req.Params, &q); err != nil {
		return Result{}, err
	}
	if err := normalizeActivityQuery(&q); err != nil {
		return Result{}, err
	}
	page, err := queryActivity(ctx, q)
	if err != nil {
		return Result{}, activityQueryError(err)
	}
	return Result{Data: decorateActivityPage(page)}, nil
}

// handleActivityShow returns one operation, request or job and its steps and
// refusals. Exactly one correlation id is required: an unbounded history dump
// is activity list's job, not show's.
func handleActivityShow(ctx context.Context, env *Env) (Result, error) {
	var q activity.Query
	if err := decodeParams(env.Req.Params, &q); err != nil {
		return Result{}, err
	}
	if q.Limit == 0 {
		q.Limit = activityShowDefaultLimit
		if q.Limit > activity.MaxLimit {
			q.Limit = activity.MaxLimit
		}
	}
	if err := normalizeActivityQuery(&q); err != nil {
		return Result{}, err
	}
	ids := 0
	for _, id := range []string{q.OperationID, q.RequestID, q.JobID} {
		if id != "" {
			ids++
		}
	}
	if ids != 1 {
		return Result{}, output.NewError(
			output.CodeUsage,
			"activity show needs exactly one of operation_id, request_id or job_id",
			output.ExitUsage,
			"Pass one correlation id from an activity list result.",
		)
	}
	page, err := queryActivity(ctx, q)
	if err != nil {
		return Result{}, activityQueryError(err)
	}
	return Result{Data: decorateActivityPage(page)}, nil
}

func handleActivityHealth(ctx context.Context, env *Env) (Result, error) {
	var p struct{}
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	store, err := activityStoreFactory()
	if err != nil || store == nil {
		return Result{}, activityReadError()
	}
	qctx, cancel := activityReadContext(ctx)
	defer cancel()
	health, err := store.Health(qctx)
	if err != nil {
		return Result{}, activityReadError()
	}
	if health.Node == "" {
		health.Node = activityNodeName()
	}
	return Result{Data: health}, nil
}

// decorateActivityPage names the node whose retained evidence the page
// covers. The store does not know its node identity, so the dispatcher adds it.
func decorateActivityPage(page activity.Page) activity.Page {
	if page.Schema == "" {
		page.Schema = activity.Schema
	}
	if page.Coverage.Node == "" {
		page.Coverage.Node = activityNodeName()
	}
	return page
}

// activityNodeName reads only the fixed node identity files, never request
// data or an arbitrary path.
func activityNodeName() string {
	return loadActivityNodeInfo(defaultActivityPaths, config.Cluster{}).Node
}

// queryActivity runs one bounded page read against the node's store.
func queryActivity(ctx context.Context, q activity.Query) (activity.Page, error) {
	store, err := activityStoreFactory()
	if err != nil || store == nil {
		return activity.Page{}, activityReadError()
	}
	qctx, cancel := activityReadContext(ctx)
	defer cancel()
	page, err := store.Query(qctx, q)
	if err != nil {
		// The handler classifies caller-caused query errors (invalid cursor or
		// filter mismatch) separately from an unreadable store. Do not flatten
		// both into activity_unavailable here.
		return activity.Page{}, err
	}
	return page, nil
}

func activityReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, activityQueryTimeout)
}

// activityReadError keeps store error text out of the RPC envelope; the
// reason is the stable token doctor and operators key on.
func activityReadError() error {
	return output.NewError(
		output.CodePrecondition,
		"the node's activity history could not be read",
		output.ExitPrecondition,
		"Run vaire doctor --only host and inspect the node's activity storage permissions, free space and health; no operation was retried.",
	).WithDetails(map[string]any{"reason": "activity_unavailable"})
}

// activityQueryError distinguishes a bad caller query from an unreadable store.
// The core classifier never returns the rejected value or raw error text, so a
// filter, cursor or limit cannot turn an agent-facing error into free-form
// diagnostic data.
func activityQueryError(err error) error {
	reason, ok := activity.ClassifyQueryError(err)
	if !ok {
		return activityReadError()
	}
	message := "the activity query is not valid"
	switch reason {
	case activity.QueryReasonInvalidCursor:
		message = "the activity cursor is not valid"
	case activity.QueryReasonCursorMismatch:
		message = "the activity cursor does not match these filters, limit or store"
	case activity.QueryReasonOutOfRange:
		message = "an activity filter value is out of range"
	}
	return output.NewError(output.CodeUsage, message, output.ExitUsage,
		"Use vaire activity list with bounded filters and a cursor from the same query.").
		WithDetails(map[string]any{"reason": reason})
}

// normalizeActivityQuery validates and normalizes filters before the store
// sees them. Unknown JSON fields were already rejected by decodeParams.
func normalizeActivityQuery(q *activity.Query) error {
	q.OperationID = strings.ToUpper(strings.TrimSpace(q.OperationID))
	if q.OperationID != "" && safeActivityID(q.OperationID) == "" {
		return activityQueryUsage("operation_id must be a ULID")
	}
	q.RequestID = strings.ToUpper(strings.TrimSpace(q.RequestID))
	if q.RequestID != "" && safeActivityID(q.RequestID) == "" {
		return activityQueryUsage("request_id must be a ULID")
	}
	q.JobID = strings.ToUpper(strings.TrimSpace(q.JobID))
	if q.JobID != "" && safeActivityID(q.JobID) == "" {
		return activityQueryUsage("job_id must be a ULID")
	}

	q.Database = strings.TrimSpace(q.Database)
	if q.Database != "" && safeActivityResource(q.Database) == "" {
		return activityQueryUsage("database is not a bounded resource name")
	}
	q.Action = strings.TrimSpace(q.Action)
	if q.Action != "" && !activityQueryActionPattern.MatchString(q.Action) {
		return activityQueryUsage("action is not a bounded action name")
	}
	q.Outcome = strings.TrimSpace(q.Outcome)
	if q.Outcome != "" && !validActivityOutcome(q.Outcome) {
		return activityQueryUsage("outcome is not one of " + strings.Join(activity.Outcomes, ", "))
	}
	q.Cursor = strings.TrimSpace(q.Cursor)
	if len(q.Cursor) > 2048 || !activityCursorPattern.MatchString(q.Cursor) {
		return activityQueryUsage("cursor is not a bounded opaque value")
	}
	if q.Since != "" {
		if err := validateActivityTime(q.Since); err != nil {
			return err
		}
	}
	if q.Until != "" {
		if err := validateActivityTime(q.Until); err != nil {
			return err
		}
	}
	if q.Limit < 0 {
		return activityQueryUsage("limit must not be negative")
	}
	if q.Limit > activity.MaxLimit {
		q.Limit = activity.MaxLimit
	}
	if err := activity.ValidateQuery(*q); err != nil {
		return activityQueryUsage("the activity query is not valid")
	}
	return nil
}

func validateActivityTime(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return activityQueryUsage("since and until must be an RFC3339 time or a lookback duration")
	}
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return nil
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return nil
	}
	return activityQueryUsage("since and until must be an RFC3339 time or a positive lookback duration")
}

func validActivityOutcome(value string) bool {
	for _, outcome := range activity.Outcomes {
		if value == outcome {
			return true
		}
	}
	return false
}

func activityQueryUsage(message string) error {
	return output.NewError(output.CodeUsage, message, output.ExitUsage, "See activity.Query's fields for the accepted filters.")
}
