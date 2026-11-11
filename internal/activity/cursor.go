package activity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
)

// Cursor bounds. A cursor is a fixed, bounded document: it binds the filters of
// the page it came from, the frozen time window, the page limit and the
// sequence highwater, so later appends can neither duplicate nor silently
// change the result.
const (
	cursorPrefix   = "c1."
	cursorVersion  = 1
	maxCursorBytes = 2048

	// cursorFieldLimit matches maxTokenLen plus some room for the JSON field
	// names and timestamps.
	cursorWindowBytes = 64
)

// cursorState is the opaque payload of Page.NextCursor. Field names are short
// so a full cursor stays well under maxCursorBytes.
type cursorState struct {
	Version int    `json:"v"`
	StoreID string `json:"i"`
	After   uint64 `json:"a"`
	// Snapshot is the sequence highwater of the page chain.
	Snapshot uint64 `json:"s"`

	SinceRaw string `json:"sr,omitempty"`
	UntilRaw string `json:"ur,omitempty"`
	SinceAt  string `json:"sa,omitempty"`
	UntilAt  string `json:"ua,omitempty"`

	OperationID string `json:"op,omitempty"`
	RequestID   string `json:"rq,omitempty"`
	JobID       string `json:"jb,omitempty"`
	Database    string `json:"db,omitempty"`
	Action      string `json:"ac,omitempty"`
	Outcome     string `json:"oc,omitempty"`

	Limit int `json:"l"`
}

// encodeCursor renders c. The payload is a fixed struct, so marshalling cannot
// fail; an empty result means "do not offer a cursor".
func encodeCursor(c cursorState) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	encoded := cursorPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > maxCursorBytes {
		return ""
	}
	return encoded
}

// decodeCursor parses and validates an opaque cursor. Every failure is
// errInvalidCursor: the caller's malformed value is never echoed.
func decodeCursor(raw string) (cursorState, error) {
	var c cursorState
	if len(raw) == 0 || len(raw) > maxCursorBytes || !strings.HasPrefix(raw, cursorPrefix) {
		return c, errInvalidCursor
	}
	payload := raw[len(cursorPrefix):]
	if len(payload) > base64.RawURLEncoding.EncodedLen(maxCursorBytes) {
		return c, errInvalidCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return c, errInvalidCursor
	}
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return cursorState{}, errInvalidCursor
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return cursorState{}, errInvalidCursor
	}
	if c.Version != cursorVersion || c.After == 0 || c.Snapshot < c.After || !validStoreID(c.StoreID) {
		return cursorState{}, errInvalidCursor
	}
	if c.Limit < 1 || c.Limit > MaxLimit {
		return cursorState{}, errInvalidCursor
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"operation_id", c.OperationID, maxTokenLen},
		{"request_id", c.RequestID, maxTokenLen},
		{"job_id", c.JobID, maxTokenLen},
		{"database", c.Database, maxDatabaseLen},
		{"action", c.Action, maxActionLen},
	} {
		if checkToken(field.name, field.value, field.max, false) != nil {
			return cursorState{}, errInvalidCursor
		}
	}
	if c.Outcome != "" && !contains(Outcomes, c.Outcome) {
		return cursorState{}, errInvalidCursor
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"since", c.SinceAt},
		{"until", c.UntilAt},
	} {
		if field.value == "" {
			continue
		}
		if _, err := parseStoredTime(field.value); err != nil {
			return cursorState{}, errInvalidCursor
		}
	}
	if c.SinceAt != "" && c.UntilAt != "" {
		since, err := parseStoredTime(c.SinceAt)
		if err != nil {
			return cursorState{}, errInvalidCursor
		}
		until, err := parseStoredTime(c.UntilAt)
		if err != nil || !since.Before(until) {
			return cursorState{}, errInvalidCursor
		}
	}
	if len(c.SinceRaw) > cursorWindowBytes || len(c.UntilRaw) > cursorWindowBytes {
		return cursorState{}, errInvalidCursor
	}
	return c, nil
}

// cursorFor builds the cursor for the next page of nq, resuming after event
// sequence after and never reading past snapshot. storeID binds the cursor to
// one store instance; without it no cursor is offered.
func cursorFor(nq normalizedQuery, after, snapshot uint64, storeID string) string {
	if after == 0 || after >= snapshot || !validStoreID(storeID) {
		return ""
	}
	c := cursorState{
		Version:     cursorVersion,
		StoreID:     storeID,
		After:       after,
		Snapshot:    snapshot,
		SinceRaw:    nq.filter.Since,
		UntilRaw:    nq.filter.Until,
		OperationID: nq.filter.OperationID,
		RequestID:   nq.filter.RequestID,
		JobID:       nq.filter.JobID,
		Database:    nq.filter.Database,
		Action:      nq.filter.Action,
		Outcome:     nq.filter.Outcome,
		Limit:       nq.limit,
	}
	if !nq.since.IsZero() {
		c.SinceAt = formatTime(nq.since)
	}
	if !nq.until.IsZero() {
		c.UntilAt = formatTime(nq.until)
	}
	return encodeCursor(c)
}
