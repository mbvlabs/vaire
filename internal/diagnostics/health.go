package diagnostics

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

var modePattern = regexp.MustCompile(`^0[0-7]{3}$`)

// ValidateHealth validates typed activity store health before it is shown to a
// human or an agent. It enforces the fixed default directory, bounded node and
// mode strings, UTC timestamps, non-negative counters and the documented
// owner-only, budget and loss relationships. Errors carry only a field name
// and a fixed reason, never the rejected value.
func ValidateHealth(h activity.Health) error {
	if err := validateHealth(h); err != nil {
		details := map[string]any{"reason": "invalid_health"}
		var ve *validationError
		if errors.As(err, &ve) {
			details["field"] = ve.field
			details["reason"] = ve.problem
		}
		return output.NewError(output.CodeValidation, "the activity health does not satisfy the typed contract",
			output.ExitConfig, "Upgrade the node with vaire node upgrade, then retry.").WithDetails(details)
	}
	return nil
}

func validateHealth(h activity.Health) error {
	if err := optionalPattern("node", h.Node, tokenPattern); err != nil {
		return err
	}
	// The node has one configured store location. An unexpected path is
	// metadata from outside the contract, not a directory we should echo.
	if h.Dir != activity.DefaultDir {
		return invalid("dir", "not_the_default")
	}
	if err := optionalPattern("dir_mode", h.DirMode, modePattern); err != nil {
		return err
	}
	if err := optionalPattern("segment_mode", h.SegmentMode, modePattern); err != nil {
		return err
	}
	if err := optionalTimestamp("first_retained_at", h.FirstRetainedAt); err != nil {
		return err
	}
	if err := optionalTimestamp("last_retained_at", h.LastRetainedAt); err != nil {
		return err
	}
	if err := optionalTimestamp("last_write_at", h.LastWriteAt); err != nil {
		return err
	}
	first, err := parseOptionalTime(h.FirstRetainedAt)
	if err != nil {
		return invalid("first_retained_at", "not_rfc3339")
	}
	last, err := parseOptionalTime(h.LastRetainedAt)
	if err != nil {
		return invalid("last_retained_at", "not_rfc3339")
	}
	if !first.IsZero() && !last.IsZero() && first.After(last) {
		return invalid("first_retained_at", "after_last_retained_at")
	}
	if h.MaxBytes < 0 || h.UsedBytes < 0 || h.SegmentBytes < 0 || h.RetentionS < 0 {
		return invalid("bytes", "negative")
	}
	if h.Segments < 0 || h.NonOwnerOnlySegments < 0 {
		return invalid("segments", "negative")
	}
	if h.Segments < h.NonOwnerOnlySegments {
		return invalid("non_owner_only_segments", "more_than_segments")
	}
	if h.MalformedSegments < 0 || h.MalformedEvents < 0 {
		return invalid("malformed", "negative")
	}
	if h.MalformedSegments > 0 || h.MalformedEvents > 0 || h.DroppedEvents > 0 || h.SequenceGaps > 0 {
		if !h.LossDetected {
			return invalid("loss_detected", "required")
		}
	}
	if (h.UsedBytes > h.MaxBytes) != h.OverBudget {
		return invalid("over_budget", "does_not_match_used_bytes")
	}
	if h.OwnerOnly {
		if !h.Exists {
			return invalid("owner_only", "without_store")
		}
		if h.NonOwnerOnlySegments != 0 {
			return invalid("owner_only", "with_non_owner_only_segments")
		}
		if h.DirMode != "" && !ownerOnlyMode(h.DirMode) {
			return invalid("dir_mode", "not_owner_only")
		}
		if h.SegmentMode != "" && !ownerOnlyMode(h.SegmentMode) {
			return invalid("segment_mode", "not_owner_only")
		}
	}
	return nil
}

func ownerOnlyMode(value string) bool {
	mode, err := strconv.ParseUint(value, 8, 32)
	return err == nil && mode&0o077 == 0
}
