// Package activity defines the typed, local activity-history contract shared by
// the node, the CLI and incident bundles. It records allowlisted operational
// metadata only: no raw SQL, result rows, command output, request or result
// payloads and no free-form messages.
//
// The node emits Event values and owns their identities. A Store assigns the
// schema, event ID, occurrence time and sequence when it appends. Query and
// Page describe bounded reads plus the evidence coverage that qualifies them.
// The typed contract and validation are pure. The Store reads and writes only
// the private activity directory; it touches no database or network.
//
// The on-disk store is a private, versioned format: NDJSON events and state.json
// are decoded with this package's bounded allowlist and unknown fields are
// rejected. An older build therefore refuses a record carrying fields it does
// not know instead of silently dropping them, so adding a stored field is a
// store-format change rather than an optional-field-compatible one.
//
// A missing or corrupt state file with retained history is reported as lost
// evidence by reads; Append refuses it. Recovering that state is an operator
// action, never an automatic repair or rebaseline by this package.
//
// The Store implementation in this package provides:
//
//	func DefaultStore() *Store
//	func (s *Store) Append(ctx context.Context, ev Event) (Event, error)
//	func (s *Store) Query(ctx context.Context, q Query) (Page, error)
//	func (s *Store) Health(ctx context.Context) (Health, error)
//	func ValidateEvent(ev Event) error
//	func ValidateQuery(q Query) error
//
// Append persists ev and returns it with Schema, EventID, OccurredAt and
// Sequence filled in. Query reads only inside the store's byte, segment and
// retention bounds. ValidateEvent enforces the field allowlist, token and size
// bounds, the Source, Outcome and ActorKind enumerations below, and rejects
// arbitrary or free-form content. ValidateQuery normalizes Since, Until, Limit
// and Cursor.
package activity

import "time"

// Schema versions the event and page documents. Later versions may only add
// optional fields.
const Schema = "vaire-activity/v1"

// DefaultDir is the node's owner-only activity directory.
const DefaultDir = "/var/log/vaire/activity"

// DefaultLimit applies when Query.Limit is zero.
const DefaultLimit = 100

// MaxLimit caps Query.Limit.
const MaxLimit = 500

// Event sources.
const (
	SourceRPC      = "rpc"
	SourceJob      = "job"
	SourceStep     = "step"
	SourceService  = "service"
	SourceOperator = "operator"
)

// Sources lists the allowed Event.Source values.
var Sources = []string{SourceRPC, SourceJob, SourceStep, SourceService, SourceOperator}

// Event outcomes. started is the intent or start of work; a missing terminal
// event means unknown, not success.
const (
	OutcomeStarted   = "started"
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeRefused   = "refused"
	OutcomeSkipped   = "skipped"
	OutcomeUnknown   = "unknown"
)

// Outcomes lists the allowed Event.Outcome values.
var Outcomes = []string{OutcomeStarted, OutcomeSucceeded, OutcomeFailed, OutcomeRefused, OutcomeSkipped, OutcomeUnknown}

// Actor kinds. operator is a claimed label until identities are bound to
// authenticated sessions, so ActorVerified stays false.
const (
	ActorOperator = "operator"
	ActorTimer    = "timer"
	ActorSystem   = "system"
)

// ActorKinds lists the allowed Event.ActorKind values.
var ActorKinds = []string{ActorOperator, ActorTimer, ActorSystem}

// Event is one recorded activity entry. Every field is typed and bounded;
// there is no open-ended payload. Empty optional fields are omitted from JSON.
//
// The node supplies Server, Node, NodeInstanceID, BootID, correlation IDs,
// Source, Action, Step, Database, Outcome, ErrorCode, ReasonCode, DurationMS,
// actor fields and NodeVersion. The Store supplies Schema, EventID, OccurredAt
// and Sequence, so replay never substitutes the replay time for the occurrence
// time.
type Event struct {
	Schema     string `json:"schema"`
	EventID    string `json:"event_id"`
	OccurredAt string `json:"occurred_at"`
	// Sequence orders appends within one store on one node. It is not a global
	// order across nodes.
	Sequence uint64 `json:"sequence"`

	Server         string `json:"server,omitempty"`
	Node           string `json:"node,omitempty"`
	NodeInstanceID string `json:"node_instance_id,omitempty"`
	BootID         string `json:"boot_id,omitempty"`

	// OperationID spans one logical operation across attempts, steps and later
	// nodes. RequestID identifies one RPC attempt; a retry gets a new one.
	OperationID     string `json:"operation_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	ParentRequestID string `json:"parent_request_id,omitempty"`
	JobID           string `json:"job_id,omitempty"`

	Source   string `json:"source"`
	Action   string `json:"action"`
	Step     string `json:"step,omitempty"`
	Database string `json:"database,omitempty"`

	Outcome    string `json:"outcome"`
	ErrorCode  string `json:"error_code,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`

	// DurationMS is omitted for started events and when it was not recorded; a
	// zero value is not proof of sub-millisecond work.
	DurationMS int64 `json:"duration_ms,omitempty"`

	ActorKind string `json:"actor_kind,omitempty"`
	// ActorLabel is the claimed or observed label and is never proof of
	// identity by itself.
	ActorLabel    string `json:"actor_label,omitempty"`
	ActorVerified bool   `json:"actor_verified"`

	// NodeVersion is the version and commit that emitted the event.
	NodeVersion string `json:"node_version,omitempty"`
}

// Query is one bounded read request. Since and Until accept an RFC3339
// timestamp or a lookback duration such as "24h"; the store resolves relative
// values against its own clock and normalizes everything to UTC. Since is
// inclusive, Until is exclusive, and an empty value is unbounded.
//
// Correlation and resource fields are exact matches. Limit zero means
// DefaultLimit; values above MaxLimit are capped. Cursor is the opaque,
// stable NextCursor from a previous page; the store must not silently restart
// from the beginning when it can no longer be resolved. A cursor belongs to the
// store instance that issued it: a recreated store rejects it as a mismatch,
// and evidence removed by retention reports Coverage.CursorExpired.
type Query struct {
	Since       string `json:"since,omitempty"`
	Until       string `json:"until,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	JobID       string `json:"job_id,omitempty"`
	Database    string `json:"database,omitempty"`
	Action      string `json:"action,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

// Page is one bounded result. Schema is always Schema. NextCursor is empty only
// when no further matching evidence may exist in the retained store.
type Page struct {
	Schema     string   `json:"schema"`
	Events     []Event  `json:"events"`
	Coverage   Coverage `json:"coverage"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// Coverage qualifies what a Page does and does not prove. Counters describe
// only the bounded scan that produced this page, not the whole history.
// Empty timestamps mean unknown, never "no events ever happened". Available is
// false when the node's activity store could not be read at all; in that case
// empty Events is missing evidence, not a clean result.
//
// Partial is set when requested evidence is unavailable: the node could not be
// read, the window reaches before FirstRetainedAt, the cursor expired, or
// scanned evidence shows loss. It is not set merely because nothing matched.
//
// Truncated is set when a scan bound (bytes, segments or event count) stopped
// before the requested window was exhausted. LimitReached is set when
// EventsReturned reached the effective limit. NextCursor is set whenever more
// matching evidence may be available.
//
// CursorExpired is set when the caller's Cursor pointed into evidence removed
// by rotation or retention; a caller must not read that as an empty history.
// LossDetected summarizes dropped, malformed or sequence-gap evidence in the
// scanned range.
type Coverage struct {
	Node      string `json:"node,omitempty"`
	Available bool   `json:"available"`

	// Retained bounds are the store's oldest and newest retained event times.
	FirstRetainedAt string `json:"first_retained_at,omitempty"`
	LastRetainedAt  string `json:"last_retained_at,omitempty"`
	// Returned bounds are the first and last event times on this page.
	FirstReturnedAt string `json:"first_returned_at,omitempty"`
	LastReturnedAt  string `json:"last_returned_at,omitempty"`

	Partial       bool `json:"partial"`
	Truncated     bool `json:"truncated"`
	LimitReached  bool `json:"limit_reached"`
	CursorExpired bool `json:"cursor_expired"`
	LossDetected  bool `json:"loss_detected"`

	// EventsReturned equals len(Events); EventsScanned counts events examined
	// before filtering. BytesScanned and SegmentsScanned bound the read.
	EventsReturned  int   `json:"events_returned"`
	EventsScanned   int   `json:"events_scanned"`
	SegmentsScanned int   `json:"segments_scanned"`
	BytesScanned    int64 `json:"bytes_scanned"`

	MalformedSegments int    `json:"malformed_segments"`
	MalformedEvents   int    `json:"malformed_events"`
	DroppedEvents     uint64 `json:"dropped_events"`
	SequenceGaps      uint64 `json:"sequence_gaps"`
}

// Health is typed store metadata for doctor and diagnostics. It carries no
// free-form error text: a check reports the concrete condition from these
// fields instead of echoing a message.
//
// Exists false means the path is absent; IsDir, Readable, Writable and the
// mode fields then describe only what could be observed. DirMode and
// SegmentMode are octal strings such as "0700" and "0600"; SegmentMode is the
// newest segment's mode. OwnerOnly means the directory and every segment are
// owner-only, and NonOwnerOnlySegments counts segments that are not.
//
// MaxBytes is the whole-store byte budget, SegmentBytes the target segment
// size, UsedBytes the current total and Segments the segment count.
// RetentionS is the configured maximum age. OverBudget is UsedBytes > MaxBytes.
//
// FirstRetainedAt and LastRetainedAt bound the retained evidence;
// LastWriteAt is the newest successful append. MalformedSegments and
// MalformedEvents count unparseable data, DroppedEvents counts append failures
// the store recorded and SequenceGaps counts missing sequence numbers.
// LossDetected summarizes those four counters.
type Health struct {
	Node string `json:"node,omitempty"`
	Dir  string `json:"dir"`

	Exists   bool   `json:"exists"`
	IsDir    bool   `json:"is_dir"`
	DirMode  string `json:"dir_mode,omitempty"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`

	SegmentMode          string `json:"segment_mode,omitempty"`
	OwnerOnly            bool   `json:"owner_only"`
	NonOwnerOnlySegments int    `json:"non_owner_only_segments"`

	MaxBytes     int64 `json:"max_bytes"`
	UsedBytes    int64 `json:"used_bytes"`
	SegmentBytes int64 `json:"segment_bytes"`
	Segments     int   `json:"segments"`
	RetentionS   int64 `json:"retention_s"`
	OverBudget   bool  `json:"over_budget"`

	FirstRetainedAt string `json:"first_retained_at,omitempty"`
	LastRetainedAt  string `json:"last_retained_at,omitempty"`
	LastWriteAt     string `json:"last_write_at,omitempty"`

	MalformedSegments int    `json:"malformed_segments"`
	MalformedEvents   int    `json:"malformed_events"`
	DroppedEvents     uint64 `json:"dropped_events"`
	SequenceGaps      uint64 `json:"sequence_gaps"`
	LossDetected      bool   `json:"loss_detected"`
}

// Store is the local activity store's configuration. DefaultStore, Append,
// Query, Health, ValidateEvent and ValidateQuery in this package implement the
// contract.
//
// Dir is the owner-only directory, MaxBytes the whole-store byte budget,
// SegmentBytes the target size of one NDJSON segment and Retention the maximum
// age. A zero Dir means DefaultDir; zero budgets and retention take the
// documented defaults (32 MiB, 1 MiB, 14 days).
type Store struct {
	Dir          string
	MaxBytes     int64
	SegmentBytes int64
	Retention    time.Duration
}
