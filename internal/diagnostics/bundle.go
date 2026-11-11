package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

// The bundle is deliberately narrow: identifiers, versions, timestamps and
// the typed activity page. Every string that reaches a bundle is checked
// against one of these bounded shapes, so a hostile node reply cannot turn a
// field into SQL, a credential, a URL or a multi-line instruction.
const maxQueryCursorBytes = 2048

var (
	tokenPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	databasePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	actionPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)
	versionPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,95}$`)
	labelPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}$`)
	cursorPattern   = regexp.MustCompile(`^c1\.[A-Za-z0-9_-]+$`)
)

// validationError names a field and a stable problem, never the offending
// value: the value may itself be the secret a caller tried to smuggle in.
type validationError struct {
	field   string
	problem string
}

func (e *validationError) Error() string {
	return e.field + ": " + e.problem
}

func invalid(field, problem string) error {
	return &validationError{field: field, problem: problem}
}

// Write creates path with the canonical typed bundle as owner-only JSON. It
// never overwrites an existing file, never follows a symlink, and removes the
// file it created if writing, syncing or closing fails.
func Write(path string, b Bundle) error {
	if err := checkWritePath(path); err != nil {
		return err
	}
	if b.Activity.Events == nil {
		b.Activity.Events = []activity.Event{}
	}
	if err := validateBundle(b); err != nil {
		return writeInvalidError(err)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return output.NewError(output.CodeValidation, "the incident bundle cannot be encoded", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "encode_failed"})
	}
	data = append(data, '\n')
	if int64(len(data)) > MaxBundleBytes {
		return tooLargeError()
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator chose the bundle path.
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return output.NewError(output.CodePrecondition, "the output file already exists", output.ExitPrecondition,
				"Choose a new --out path; vaire never overwrites an incident bundle.").
				WithDetails(map[string]any{"reason": "output_exists"})
		}
		return output.NewError(output.CodeValidation, "cannot create the incident bundle", output.ExitConfig,
			"Check that the parent directory exists and is writable.").
			WithDetails(map[string]any{"reason": "create_failed"})
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return output.NewError(output.CodeValidation, "cannot inspect the new incident bundle", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "stat_failed"})
	}
	// The create mode is already 0600; chmod makes the contract independent of
	// a restrictive or unusual umask.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		if current, lerr := os.Lstat(path); lerr == nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
		return output.NewError(output.CodeValidation, "cannot set the incident bundle's permissions", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "chmod_failed"})
	}
	cleanup := func() {
		// Only remove the file this call created, even if another process
		// replaced the path meanwhile.
		if current, lerr := os.Lstat(path); lerr == nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return output.NewError(output.CodeValidation, "cannot write the incident bundle", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "write_failed"})
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return output.NewError(output.CodeValidation, "cannot sync the incident bundle", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "sync_failed"})
	}
	if err := f.Close(); err != nil {
		cleanup()
		return output.NewError(output.CodeValidation, "cannot close the incident bundle", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "close_failed"})
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		cleanup()
		return output.NewError(output.CodeValidation, "cannot sync the incident bundle's directory", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "directory_sync_failed"})
	}
	return nil
}

// Read loads and validates one owner-only incident bundle. It enforces the
// size cap before decoding and rejects unknown fields at every nesting level,
// trailing JSON, symlinks and files another user could read.
func Read(path string) (Bundle, error) {
	var b Bundle
	if strings.TrimSpace(path) == "" {
		return b, bundleNotFound()
	}
	if hasControl(path) {
		return b, output.NewError(output.CodeValidation, "the incident bundle path contains a control character", output.ExitConfig,
			"Pass a plain file path.").
			WithDetails(map[string]any{"reason": "path_control_character"})
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, bundleNotFound()
	}
	if err != nil {
		return b, corruptError("unreadable")
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return b, output.NewError(output.CodeValidation, "the incident bundle is a symlink", output.ExitConfig,
			"Read the bundle file itself, not a symlink.").
			WithDetails(map[string]any{"reason": "symlink"})
	}
	if !before.Mode().IsRegular() {
		return b, corruptError("not_regular_file")
	}
	if before.Size() > MaxBundleBytes {
		return b, tooLargeError()
	}
	if before.Mode().Perm()&0o077 != 0 {
		return b, notOwnerOnly()
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator chose the bundle path.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return b, bundleNotFound()
		}
		return b, corruptError("unreadable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return b, corruptError("changed_while_opening")
	}
	if info.Size() > MaxBundleBytes {
		return b, tooLargeError()
	}
	if info.Mode().Perm()&0o077 != 0 {
		return b, notOwnerOnly()
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBundleBytes+1))
	if err != nil {
		return b, corruptError("unreadable")
	}
	if int64(len(data)) > MaxBundleBytes {
		return b, tooLargeError()
	}
	if err := decodeStrict(data, &b); err != nil {
		return b, err
	}
	if err := validateBundle(b); err != nil {
		return b, BundleInvalidError(err)
	}
	// A JSON null or missing events array is accepted as the canonical empty
	// array. Missing history stays available=false/partial=true; it is not a
	// corrupt page.
	if b.Activity.Events == nil {
		b.Activity.Events = []activity.Event{}
	}
	return b, nil
}

func decodeStrict(data []byte, b *Bundle) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(b); err != nil {
		reason := "decode_failed"
		if strings.Contains(err.Error(), "unknown field") {
			reason = "unknown_field"
		}
		return corruptError(reason)
	}
	// A second value, even {}, is trailing content and makes the document
	// ambiguous. Only whitespace may follow the bundle.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return corruptError("trailing_data")
	}
	return nil
}

func checkWritePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return output.NewError(output.CodeUsage, "an output file is required", output.ExitUsage,
			"Pass --out <file>.")
	}
	if path == "-" {
		return output.NewError(output.CodeUsage, "--out must be a file, not standard output", output.ExitUsage,
			"Choose a local file path for the incident bundle.")
	}
	if hasControl(path) {
		return output.NewError(output.CodeValidation, "the output path contains a control character", output.ExitConfig,
			"Choose a plain file path.").
			WithDetails(map[string]any{"reason": "path_control_character"})
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return output.NewError(output.CodeValidation, "the output path is a symlink", output.ExitConfig,
				"Choose a new file path; vaire never follows a symlink for an incident bundle.").
				WithDetails(map[string]any{"reason": "symlink"})
		}
		return output.NewError(output.CodePrecondition, "the output file already exists", output.ExitPrecondition,
			"Choose a new --out path; vaire never overwrites an incident bundle.").
			WithDetails(map[string]any{"reason": "output_exists"})
	} else if !errors.Is(err, os.ErrNotExist) {
		return output.NewError(output.CodeValidation, "cannot inspect the output path", output.ExitConfig, "").
			WithDetails(map[string]any{"reason": "path_invalid"})
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return output.NewError(output.CodeValidation, "the output directory does not exist", output.ExitConfig,
			"Create the parent directory first; vaire does not create directory trees for a bundle.").
			WithDetails(map[string]any{"reason": "parent_missing"})
	}
	return nil
}

// CheckWritePath reports whether path can become a new owner-only bundle
// without writing one. It is the same preflight Write performs, exposed so a
// dry run validates the target before contacting a node.
func CheckWritePath(path string) error {
	return checkWritePath(path)
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the parent of the bundle path.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func validateBundle(b Bundle) error {
	if b.Schema != Schema {
		return invalid("schema", "unsupported")
	}
	if err := requireTimestamp("created_at", b.CreatedAt); err != nil {
		return err
	}
	if err := requirePattern("metadata.server", b.Metadata.Server, tokenPattern); err != nil {
		return err
	}
	if err := requirePattern("metadata.node", b.Metadata.Node, tokenPattern); err != nil {
		return err
	}
	if err := requirePattern("metadata.node_version", b.Metadata.NodeVersion, versionPattern); err != nil {
		return err
	}
	if err := requirePattern("metadata.client_version", b.Metadata.ClientVersion, versionPattern); err != nil {
		return err
	}
	if err := requireTimestamp("metadata.collected_at", b.Metadata.CollectedAt); err != nil {
		return err
	}
	createdAt, _ := parseOptionalTime(b.CreatedAt)
	collectedAt, _ := parseOptionalTime(b.Metadata.CollectedAt)
	if createdAt.Before(collectedAt) {
		return invalid("created_at", "before_collected_at")
	}
	if err := validateQuery(b.Query, collectedAt); err != nil {
		return err
	}
	return validatePage(b.Activity)
}

// ValidatePage validates a typed activity page with the same allowlist and
// coverage invariants a bundle uses. It returns a safe validation_failed error
// that names only the field and a fixed reason, never the rejected value.
func ValidatePage(p activity.Page) error {
	if err := validatePage(p); err != nil {
		return validationFailedError(err)
	}
	return nil
}

// validateQuery constrains the optional stored activity filters. It is a
// typed activity.Query, never a map: the core validator covers identifier,
// enum, timestamp and cursor semantics, while the bundle layer rejects a
// Limit above the activity maximum and an over-long cursor before that.
func validateQuery(q *activity.Query, collectedAt time.Time) error {
	if q == nil {
		return nil
	}
	if q.Limit < 0 || q.Limit > activity.MaxLimit {
		return invalid("query.limit", "out_of_range")
	}
	if len(q.Cursor) > maxQueryCursorBytes {
		return invalid("query.cursor", "too_long")
	}
	if q.Cursor != "" && !cursorPattern.MatchString(q.Cursor) {
		return invalid("query.cursor", "not_allowed")
	}
	if err := activity.ValidateQueryAt(*q, collectedAt); err != nil {
		return invalid("query", "invalid")
	}
	return nil
}

func validatePage(p activity.Page) error {
	if p.Schema != activity.Schema {
		return invalid("activity.schema", "unsupported")
	}
	c := p.Coverage
	if err := optionalPattern("activity.coverage.node", c.Node, tokenPattern); err != nil {
		return err
	}
	if len(p.Events) > activity.MaxLimit {
		return invalid("activity.events", "too_many")
	}
	if !c.Available && len(p.Events) > 0 {
		return invalid("activity.coverage.available", "false_with_events")
	}
	if !c.Available && p.NextCursor != "" {
		return invalid("activity.next_cursor", "unavailable_with_cursor")
	}
	if c.EventsReturned != len(p.Events) {
		return invalid("activity.coverage.events_returned", "does_not_match_events")
	}
	if c.EventsScanned < c.EventsReturned {
		return invalid("activity.coverage.events_scanned", "less_than_returned")
	}
	if c.SegmentsScanned < 0 || c.BytesScanned < 0 ||
		c.MalformedSegments < 0 || c.MalformedEvents < 0 {
		return invalid("activity.coverage.counters", "negative")
	}
	if c.MalformedSegments > 0 || c.MalformedEvents > 0 || c.DroppedEvents > 0 || c.SequenceGaps > 0 {
		if !c.LossDetected {
			return invalid("activity.coverage.loss_detected", "required")
		}
	}
	if !c.Available || c.CursorExpired || c.LossDetected {
		if !c.Partial {
			return invalid("activity.coverage.partial", "required")
		}
	}
	if c.LimitReached && c.EventsReturned == 0 {
		return invalid("activity.coverage.limit_reached", "without_events")
	}
	if err := optionalTimestamp("activity.coverage.first_retained_at", c.FirstRetainedAt); err != nil {
		return err
	}
	if err := optionalTimestamp("activity.coverage.last_retained_at", c.LastRetainedAt); err != nil {
		return err
	}
	firstRetained, err := parseOptionalTime(c.FirstRetainedAt)
	if err != nil {
		return err
	}
	lastRetained, err := parseOptionalTime(c.LastRetainedAt)
	if err != nil {
		return err
	}
	if !firstRetained.IsZero() && !lastRetained.IsZero() && firstRetained.After(lastRetained) {
		return invalid("activity.coverage.first_retained_at", "after_last_retained_at")
	}
	if err := optionalTimestamp("activity.coverage.first_returned_at", c.FirstReturnedAt); err != nil {
		return err
	}
	if err := optionalTimestamp("activity.coverage.last_returned_at", c.LastReturnedAt); err != nil {
		return err
	}
	firstReturned, err := parseOptionalTime(c.FirstReturnedAt)
	if err != nil {
		return err
	}
	lastReturned, err := parseOptionalTime(c.LastReturnedAt)
	if err != nil {
		return err
	}
	if len(p.Events) == 0 {
		if c.FirstReturnedAt != "" || c.LastReturnedAt != "" {
			return invalid("activity.coverage.first_returned_at", "without_events")
		}
	} else {
		minimum, maximum, err := eventBounds(p.Events)
		if err != nil {
			return err
		}
		if c.FirstReturnedAt == "" || c.LastReturnedAt == "" {
			return invalid("activity.coverage.first_returned_at", "missing")
		}
		if !firstReturned.Equal(minimum) {
			return invalid("activity.coverage.first_returned_at", "does_not_match_events")
		}
		if !lastReturned.Equal(maximum) {
			return invalid("activity.coverage.last_returned_at", "does_not_match_events")
		}
	}
	if err := validateCursor("activity.next_cursor", p.NextCursor); err != nil {
		return err
	}
	for i, ev := range p.Events {
		if err := validateEvent(fmt.Sprintf("activity.events[%d]", i), ev); err != nil {
			return err
		}
	}
	return nil
}

func eventBounds(events []activity.Event) (time.Time, time.Time, error) {
	var minimum, maximum time.Time
	for i, ev := range events {
		if err := requireTimestamp(fmt.Sprintf("activity.events[%d].occurred_at", i), ev.OccurredAt); err != nil {
			return time.Time{}, time.Time{}, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ev.OccurredAt)
		if i == 0 || t.Before(minimum) {
			minimum = t
		}
		if i == 0 || t.After(maximum) {
			maximum = t
		}
	}
	return minimum, maximum, nil
}

func validateEvent(field string, ev activity.Event) error {
	if ev.Schema != activity.Schema {
		return invalid(field+".schema", "unsupported")
	}
	if err := requirePattern(field+".event_id", ev.EventID, idPattern); err != nil {
		return err
	}
	if err := requireTimestamp(field+".occurred_at", ev.OccurredAt); err != nil {
		return err
	}
	if ev.Sequence == 0 {
		return invalid(field+".sequence", "missing")
	}
	if !contains(activity.Sources, ev.Source) {
		return invalid(field+".source", "unknown")
	}
	if !contains(activity.Outcomes, ev.Outcome) {
		return invalid(field+".outcome", "unknown")
	}
	if ev.ActorKind != "" && !contains(activity.ActorKinds, ev.ActorKind) {
		return invalid(field+".actor_kind", "unknown")
	}
	if err := requirePattern(field+".action", ev.Action, actionPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".server", ev.Server, tokenPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".node", ev.Node, tokenPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".node_instance_id", ev.NodeInstanceID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".boot_id", ev.BootID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".operation_id", ev.OperationID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".request_id", ev.RequestID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".parent_request_id", ev.ParentRequestID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".job_id", ev.JobID, idPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".step", ev.Step, actionPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".database", ev.Database, databasePattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".error_code", ev.ErrorCode, tokenPattern); err != nil {
		return err
	}
	if err := optionalPattern(field+".reason_code", ev.ReasonCode, tokenPattern); err != nil {
		return err
	}
	if ev.DurationMS < 0 {
		return invalid(field+".duration_ms", "negative")
	}
	if err := optionalPattern(field+".actor_label", ev.ActorLabel, labelPattern); err != nil {
		return err
	}
	if ev.ActorLabel != "" && ev.ActorKind == "" {
		return invalid(field+".actor_label", "needs_actor_kind")
	}
	if ev.ActorVerified && ev.ActorKind != activity.ActorTimer && ev.ActorKind != activity.ActorSystem {
		return invalid(field+".actor_verified", "cannot_verify_operator")
	}
	if err := optionalPattern(field+".node_version", ev.NodeVersion, versionPattern); err != nil {
		return err
	}
	// Keep the bundle/event contract aligned with the core emitter and store
	// validator. Its errors are fixed tokens, and this wrapper reports only a
	// stable field/reason pair.
	if err := activity.ValidateEvent(ev); err != nil {
		return invalid(field, "violates_activity_contract")
	}
	return nil
}

func validateCursor(field, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 2048 || !cursorPattern.MatchString(value) {
		return invalid(field, "not_allowed")
	}
	return nil
}

func requirePattern(field, value string, pattern *regexp.Regexp) error {
	if value == "" {
		return invalid(field, "missing")
	}
	if !pattern.MatchString(value) {
		return invalid(field, "not_allowed")
	}
	return nil
}

func optionalPattern(field, value string, pattern *regexp.Regexp) error {
	if value == "" {
		return nil
	}
	if !pattern.MatchString(value) {
		return invalid(field, "not_allowed")
	}
	return nil
}

func requireTimestamp(field, value string) error {
	if value == "" {
		return invalid(field, "missing")
	}
	return validateTimestamp(field, value)
}

func optionalTimestamp(field, value string) error {
	if value == "" {
		return nil
	}
	return validateTimestamp(field, value)
}

func validateTimestamp(field, value string) error {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return invalid(field, "not_rfc3339")
	}
	if t.Year() < 1970 || t.Year() > 9999 {
		return invalid(field, "year_out_of_range")
	}
	// Only the UTC Z form is canonical in a bundle; a numeric offset is
	// accepted by RFC3339 but makes the same instant look different.
	if !strings.HasSuffix(value, "Z") {
		return invalid(field, "not_utc")
	}
	return nil
}

func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// BundleInvalidError converts a field validation failure into a safe CLI
// error. It never includes the rejected value.
func BundleInvalidError(err error) error {
	details := map[string]any{"reason": "invalid_bundle"}
	var ve *validationError
	if errors.As(err, &ve) {
		details["field"] = ve.field
		details["reason"] = ve.problem
	}
	return output.NewError(output.CodeBundleCorrupt, "the incident bundle does not satisfy the typed contract",
		output.ExitSecrets, "Collect a new bundle with vaire diagnostics collect.").WithDetails(details)
}

func validationFailedError(err error) error {
	details := map[string]any{"reason": "invalid_page"}
	var ve *validationError
	if errors.As(err, &ve) {
		details["field"] = ve.field
		details["reason"] = ve.problem
	}
	return output.NewError(output.CodeValidation, "the activity page does not satisfy the typed contract",
		output.ExitConfig, "Upgrade the node with vaire node upgrade, then retry.").WithDetails(details)
}

func writeInvalidError(err error) error {
	details := map[string]any{"reason": "invalid_bundle"}
	var ve *validationError
	if errors.As(err, &ve) {
		details["field"] = ve.field
		details["reason"] = ve.problem
	}
	return output.NewError(output.CodeValidation, "the incident bundle is invalid", output.ExitConfig, "").
		WithDetails(details)
}

func corruptError(reason string) error {
	return output.NewError(output.CodeBundleCorrupt, "the incident bundle is not valid typed JSON",
		output.ExitSecrets, "Read a bundle written by vaire diagnostics collect.").
		WithDetails(map[string]any{"reason": reason})
}

func bundleNotFound() error {
	return output.NewError(output.CodeBundleNotFound, "the incident bundle was not found", output.ExitPrecondition,
		"Check the path, or collect one with vaire diagnostics collect --out <file>.")
}

func notOwnerOnly() error {
	return output.NewError(output.CodePrecondition, "the incident bundle is readable by other users", output.ExitPrecondition,
		"Protect it with chmod 600 <file>; incident bundles must stay owner-only.").
		WithDetails(map[string]any{"reason": "not_owner_only"})
}

func tooLargeError() error {
	return output.NewError(output.CodeValidation, "the incident bundle is larger than the limit", output.ExitConfig,
		"Collect a smaller window or page.").
		WithDetails(map[string]any{"reason": "bundle_too_large", "max_bytes": MaxBundleBytes})
}
