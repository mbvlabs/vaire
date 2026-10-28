package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/secrets"
)

// Bounds for one event. They are deliberately small: an event is allowlisted
// operational metadata, not a log line.
const (
	// maxEventBytes caps one encoded event including its newline: 8 KiB.
	maxEventBytes = 8 << 10

	maxTokenLen    = 128
	maxActionLen   = 64
	maxCodeLen     = 64
	maxDatabaseLen = 64

	// minEventYear rejects epochs and zero-ish timestamps that cannot be real
	// occurrence times.
	minEventYear = 1970

	// maxDurationMS bounds DurationMS to ten years.
	maxDurationMS int64 = 10 * 365 * 24 * 60 * 60 * 1000

	// maxQueryLookback bounds a relative Since or Until value to ten years.
	maxQueryLookback = 10 * 365 * 24 * time.Hour

	// maxFutureSkew is how far ahead of the local clock an occurrence time
	// may be. It bounds clock skew between the node and the emitter while
	// refusing a timestamp that would pin retention or a relative query.
	maxFutureSkew = 5 * time.Minute
)

// tokenPattern is the allowlist for every event string and query filter: a
// leading letter or digit followed by identifier characters. Spaces, quotes,
// parentheses, '=', '?', '&', ':', control characters and newlines are
// excluded, so SQL text, query strings and URLs cannot be carried in a typed
// field.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+/-]*$`)

// Database identifiers can start with an underscore, including temporary
// restore names. Other metadata keeps the narrower leading letter/digit rule.
var databasePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+/-]*$`)

// timeLayout is the canonical stored form: RFC3339 in UTC with a fixed
// nanosecond width, so stored timestamps also sort lexically.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// errInvalidEvent reports an event that is outside the typed contract.
var errInvalidEvent = errors.New("activity: invalid event")

// redactor is the process-wide secret registry used as defense in depth. A
// matching value removes the whole optional field; it never rewrites a field
// into something that looks valid.
var redactor = secrets.Default

// ValidateEvent enforces the event allowlist. Append calls it before the store
// assigns Schema, EventID, OccurredAt and Sequence, so those may be empty; any
// value that is present must be valid. ActorVerified is never assigned here:
// the store copies it from the event and refuses a claimed operator identity.
func ValidateEvent(ev Event) error {
	if ev.Schema != "" && ev.Schema != Schema {
		return eventError("schema", "is not the supported version")
	}
	if err := checkToken("event_id", ev.EventID, maxTokenLen, false); err != nil {
		return err
	}
	if ev.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
		if err != nil {
			return eventError("occurred_at", "is not an RFC3339 timestamp")
		}
		if t.Year() < minEventYear || t.Year() > 9999 {
			return eventError("occurred_at", "is outside the supported years")
		}
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"server", ev.Server, maxTokenLen},
		{"node", ev.Node, maxTokenLen},
		{"node_instance_id", ev.NodeInstanceID, maxTokenLen},
		{"boot_id", ev.BootID, maxTokenLen},
		{"operation_id", ev.OperationID, maxTokenLen},
		{"request_id", ev.RequestID, maxTokenLen},
		{"parent_request_id", ev.ParentRequestID, maxTokenLen},
		{"job_id", ev.JobID, maxTokenLen},
		{"step", ev.Step, maxTokenLen},
		{"database", ev.Database, maxDatabaseLen},
		{"error_code", ev.ErrorCode, maxCodeLen},
		{"reason_code", ev.ReasonCode, maxCodeLen},
		{"node_version", ev.NodeVersion, maxTokenLen},
	} {
		if err := checkToken(field.name, field.value, field.max, false); err != nil {
			return err
		}
	}
	if err := checkEnum("source", ev.Source, Sources); err != nil {
		return err
	}
	if err := checkToken("action", ev.Action, maxActionLen, true); err != nil {
		return err
	}
	if err := checkEnum("outcome", ev.Outcome, Outcomes); err != nil {
		return err
	}
	if ev.DurationMS < 0 || ev.DurationMS > maxDurationMS {
		return eventError("duration_ms", "is out of range")
	}
	if ev.ActorKind != "" {
		if err := checkEnum("actor_kind", ev.ActorKind, ActorKinds); err != nil {
			return err
		}
	}
	if err := checkToken("actor_label", ev.ActorLabel, maxTokenLen, false); err != nil {
		return err
	}
	if ev.ActorLabel != "" && ev.ActorKind == "" {
		return eventError("actor_label", "needs an actor_kind")
	}
	if ev.ActorVerified {
		switch ev.ActorKind {
		case ActorTimer, ActorSystem:
		case ActorOperator, "":
			return eventError("actor_verified", "cannot be claimed for an operator")
		}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return eventError("event", "cannot be encoded")
	}
	if len(raw)+1 > maxEventBytes {
		return eventError("event", "is too large")
	}
	return nil
}

// ValidateQuery checks a query and normalizes its copy (Since, Until, Limit and
// Cursor); the caller's value is unchanged. Store.Query performs the same
// normalization with one fixed snapshot time and additionally verifies that a
// cursor belongs to its own store instance and has not expired.
func ValidateQuery(q Query) error {
	return ValidateQueryAt(q, time.Now().UTC())
}

// ValidateQueryAt validates a copy against an explicit reference time. Incident
// readers use collection time so a saved relative filter does not age into a
// different query when the evidence is inspected offline. Cursor windows remain
// frozen by the cursor itself, just as they do in live queries.
func ValidateQueryAt(q Query, at time.Time) error {
	if at.IsZero() || at.Year() < minEventYear || at.Year() > 9999 {
		return errors.New("activity: invalid query reference time")
	}
	_, err := normalizeQuery(q, at.UTC())
	return err
}

// normalizedQuery is a query with its time window resolved to absolute UTC
// times and its effective limit. cur is set when the caller supplied a cursor.
type normalizedQuery struct {
	filter Query
	limit  int
	since  time.Time
	until  time.Time
	cur    *cursorState
}

// normalizeQuery validates q against now and freezes relative times. A cursor
// keeps the window it was created with, so later pages cannot drift.
func normalizeQuery(q Query, now time.Time) (normalizedQuery, error) {
	nq := normalizedQuery{filter: q}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"operation_id", q.OperationID, maxTokenLen},
		{"request_id", q.RequestID, maxTokenLen},
		{"job_id", q.JobID, maxTokenLen},
		{"database", q.Database, maxDatabaseLen},
		{"action", q.Action, maxActionLen},
	} {
		if checkToken(field.name, field.value, field.max, false) != nil {
			return nq, errInvalidQuery
		}
	}
	if q.Outcome != "" && !contains(Outcomes, q.Outcome) {
		return nq, errInvalidQuery
	}
	switch {
	case q.Limit < 0:
		return nq, errQueryOutOfRange
	case q.Limit == 0:
		nq.limit = DefaultLimit
	case q.Limit > MaxLimit:
		nq.limit = MaxLimit
	default:
		nq.limit = q.Limit
	}
	if q.Since != "" {
		t, err := parseQueryBound(q.Since, now)
		if err != nil {
			return nq, errInvalidQuery
		}
		nq.since = t
	}
	if q.Until != "" {
		t, err := parseQueryBound(q.Until, now)
		if err != nil {
			return nq, errInvalidQuery
		}
		nq.until = t
	}
	if q.Cursor == "" && !nq.since.IsZero() && !nq.until.IsZero() && !nq.since.Before(nq.until) {
		return nq, errInvalidQuery
	}
	if q.Cursor != "" {
		cur, err := decodeCursor(q.Cursor)
		if err != nil {
			return nq, err
		}
		if cur.SinceRaw != q.Since || cur.UntilRaw != q.Until ||
			cur.OperationID != q.OperationID || cur.RequestID != q.RequestID ||
			cur.JobID != q.JobID || cur.Database != q.Database ||
			cur.Action != q.Action || cur.Outcome != q.Outcome ||
			cur.Limit != nq.limit {
			return nq, errCursorMismatch
		}
		nq.cur = &cur
		if cur.SinceAt != "" {
			t, err := parseStoredTime(cur.SinceAt)
			if err != nil {
				return nq, errInvalidCursor
			}
			nq.since = t
		} else {
			nq.since = time.Time{}
		}
		if cur.UntilAt != "" {
			t, err := parseStoredTime(cur.UntilAt)
			if err != nil {
				return nq, errInvalidCursor
			}
			nq.until = t
		} else {
			nq.until = time.Time{}
		}
		if !nq.since.IsZero() && !nq.until.IsZero() && !nq.since.Before(nq.until) {
			return nq, errInvalidCursor
		}
	}
	return nq, nil
}

// parseQueryBound accepts an RFC3339 timestamp or a positive lookback duration
// such as "24h", resolved against now.
func parseQueryBound(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil {
		if d <= 0 || d > maxQueryLookback {
			return time.Time{}, errInvalidQuery
		}
		return now.Add(-d).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() < minEventYear || t.Year() > 9999 {
		return time.Time{}, errInvalidQuery
	}
	return t.UTC(), nil
}

// parseStoredTime parses a timestamp written by the store or a caller cursor.
func parseStoredTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() < minEventYear || t.Year() > 9999 {
		return time.Time{}, errInvalidCursor
	}
	return t.UTC(), nil
}

// formatTime renders t in the canonical stored form.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// normalizeEventTime preserves a caller timestamp and otherwise uses now.
func normalizeEventTime(value string, now time.Time) (string, error) {
	if value == "" {
		return formatTime(now), nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() < minEventYear || t.Year() > 9999 {
		return "", errInvalidEvent
	}
	return formatTime(t), nil
}

// validateStoredEvent enforces everything a persisted event needs: the full
// allowlist plus the fields the store always assigns.
func validateStoredEvent(ev Event) error {
	if err := ValidateEvent(ev); err != nil {
		return err
	}
	if ev.Schema != Schema || ev.EventID == "" || ev.OccurredAt == "" || ev.Sequence == 0 {
		return errMalformedEvent
	}
	// MaxUint64 is never assigned: accepting it would make the next sequence
	// overflow and fabricate a huge gap.
	if ev.Sequence == ^uint64(0) {
		return errMalformedEvent
	}
	return nil
}

// checkOccurrenceSkew rejects an occurrence time more than maxFutureSkew ahead
// of now. ValidateEvent stays clock-free; Append applies this policy.
func checkOccurrenceSkew(occurred string, now time.Time) error {
	t, err := time.Parse(time.RFC3339Nano, occurred)
	if err != nil {
		return eventError("occurred_at", "is not an RFC3339 timestamp")
	}
	if t.After(now.Add(maxFutureSkew)) {
		return eventError("occurred_at", "is in the future")
	}
	return nil
}

// sanitizeEvent applies the registered-secret defense in depth: a field whose
// value contains a registered secret is removed rather than rewritten. Required
// fields then fail validation, so a secret is never persisted.
func sanitizeEvent(ev Event) Event {
	ev.Server = redactField(ev.Server)
	ev.Node = redactField(ev.Node)
	ev.NodeInstanceID = redactField(ev.NodeInstanceID)
	ev.BootID = redactField(ev.BootID)
	ev.OperationID = redactField(ev.OperationID)
	ev.RequestID = redactField(ev.RequestID)
	ev.ParentRequestID = redactField(ev.ParentRequestID)
	ev.JobID = redactField(ev.JobID)
	ev.Action = redactField(ev.Action)
	ev.Step = redactField(ev.Step)
	ev.Database = redactField(ev.Database)
	ev.ErrorCode = redactField(ev.ErrorCode)
	ev.ReasonCode = redactField(ev.ReasonCode)
	ev.ActorKind = redactField(ev.ActorKind)
	ev.ActorLabel = redactField(ev.ActorLabel)
	ev.NodeVersion = redactField(ev.NodeVersion)
	return ev
}

func redactField(value string) string {
	if value == "" {
		return ""
	}
	if redacted := redactor.Redact(value); redacted != value {
		return ""
	}
	return value
}

// checkToken validates one optional or required identifier.
func checkToken(field, value string, max int, required bool) error {
	if value == "" {
		if required {
			return eventError(field, "is required")
		}
		return nil
	}
	if len(value) > max {
		return eventError(field, "is too long")
	}
	pattern := tokenPattern
	if field == "database" {
		pattern = databasePattern
	}
	if !pattern.MatchString(value) || strings.Contains(value, "://") {
		return eventError(field, "is not an allowlisted identifier")
	}
	return nil
}

// checkEnum validates a required enumeration value.
func checkEnum(field, value string, allowed []string) error {
	if !contains(allowed, value) {
		return eventError(field, "is not an allowed value")
	}
	return nil
}

func contains(allowed []string, value string) bool {
	for _, item := range allowed {
		if item == value {
			return true
		}
	}
	return false
}

func eventError(field, reason string) error {
	return fmt.Errorf("%w: %s %s", errInvalidEvent, field, reason)
}
