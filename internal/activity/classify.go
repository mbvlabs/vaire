package activity

import "errors"

// Stable reasons returned by ClassifyQueryError for caller-caused query
// problems. They are fixed strings, contain no user input and can be mapped to
// a usage or refused error by a transport.
const (
	QueryReasonInvalid        = "invalid_query"
	QueryReasonInvalidCursor  = "invalid_cursor"
	QueryReasonCursorMismatch = "cursor_mismatch"
	QueryReasonOutOfRange     = "out_of_range"
)

// ClassifyQueryError reports whether err is a caller-caused query error and, if
// so, its stable reason. It never returns error text or user-supplied values.
//
// A cursor that points into evidence removed by retention or rotation is not an
// error: Store.Query reports it through Page.Coverage.CursorExpired. Store read,
// unsafe-path, corrupt/missing-state, configuration and internal errors are not
// classified (ok false), because they are node conditions rather than invalid
// caller input.
func ClassifyQueryError(err error) (reason string, ok bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, errInvalidQuery):
		return QueryReasonInvalid, true
	case errors.Is(err, errInvalidCursor):
		return QueryReasonInvalidCursor, true
	case errors.Is(err, errCursorMismatch):
		return QueryReasonCursorMismatch, true
	case errors.Is(err, errQueryOutOfRange):
		return QueryReasonOutOfRange, true
	default:
		return "", false
	}
}
