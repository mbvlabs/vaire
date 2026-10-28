package activity

import "errors"

// Bounded sentinel errors. Every one of them is a fixed string: a malformed
// value from a query, cursor or stored file is never echoed back.
var (
	errMalformedEvent    = errors.New("activity: malformed event")
	errInvalidQuery      = errors.New("activity: invalid query")
	errQueryOutOfRange   = errors.New("activity: query value out of range")
	errInvalidCursor     = errors.New("activity: invalid cursor")
	errCursorMismatch    = errors.New("activity: cursor does not match query")
	errConfig            = errors.New("activity: invalid store configuration")
	errUnsafeDir         = errors.New("activity: store directory is not private")
	errUnsafeSegment     = errors.New("activity: segment file is not private")
	errUnsafeState       = errors.New("activity: state file is not private")
	errUnsafeLock        = errors.New("activity: lock file is not private")
	errCorruptState      = errors.New("activity: state file is corrupt")
	errStateMissing      = errors.New("activity: state file is missing while activity history exists")
	errHistoryMissing    = errors.New("activity: recorded history is missing while the state counts its events")
	errTooManySegments   = errors.New("activity: too many segment files")
	errSequenceExhausted = errors.New("activity: sequence space exhausted")
	errLockTimeout       = errors.New("activity: timed out waiting for the store lock")
)
