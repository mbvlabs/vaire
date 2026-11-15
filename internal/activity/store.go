package activity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Store defaults. They are the node's bounded defaults; a Store field set to
// zero takes the value here.
const (
	defaultMaxBytes     int64 = 32 << 20
	defaultSegmentBytes int64 = 1 << 20
	defaultRetention          = 14 * 24 * time.Hour

	dirMode    os.FileMode = 0o700
	parentMode os.FileMode = 0o750
	fileMode   os.FileMode = 0o600

	// appendLockWait bounds how long an append waits for another process's
	// rotation or append.
	appendLockWait = 2 * time.Second
)

// lockWait and lockPoll are variables so tests can shorten contention.
var (
	lockWait = appendLockWait
	lockPoll = 2 * time.Millisecond
)

// DefaultStore returns the node's activity store configuration: the owner-only
// directory under /var/log/vaire with a 32 MiB byte budget, 1 MiB segments and
// 14 days of retention.
func DefaultStore() *Store {
	return &Store{
		Dir:          DefaultDir,
		MaxBytes:     defaultMaxBytes,
		SegmentBytes: defaultSegmentBytes,
		Retention:    defaultRetention,
	}
}

// storeConfig is Store with defaults applied and validated.
type storeConfig struct {
	dir          string
	maxBytes     int64
	segmentBytes int64
	retention    time.Duration
}

// config resolves Store fields and refuses an unusable budget. A nil Store uses
// the defaults.
func (s *Store) config() (storeConfig, error) {
	cfg := storeConfig{
		dir:          DefaultDir,
		maxBytes:     defaultMaxBytes,
		segmentBytes: defaultSegmentBytes,
		retention:    defaultRetention,
	}
	if s != nil {
		if s.Dir != "" {
			cfg.dir = filepath.Clean(s.Dir)
		}
		if s.MaxBytes != 0 {
			cfg.maxBytes = s.MaxBytes
		}
		if s.SegmentBytes != 0 {
			cfg.segmentBytes = s.SegmentBytes
		}
		if s.Retention != 0 {
			cfg.retention = s.Retention
		}
	}
	if !filepath.IsAbs(cfg.dir) || cfg.dir == string(filepath.Separator) {
		return storeConfig{}, errConfig
	}
	if cfg.retention < 0 {
		return storeConfig{}, errConfig
	}
	if cfg.segmentBytes < maxEventBytes || cfg.maxBytes < cfg.segmentBytes {
		return storeConfig{}, errConfig
	}
	if cfg.segmentBytes > hardScanCap {
		// A segment that cannot be read within the absolute page cap would
		// make bounded progress impossible; refuse the configuration instead.
		return storeConfig{}, errConfig
	}
	return cfg, nil
}

// Append persists ev and returns it with Schema, EventID, OccurredAt and
// Sequence assigned. A caller-supplied OccurredAt is preserved; any
// caller-supplied Schema, EventID or Sequence is replaced by the store's own
// identity. Append fails when the event is invalid, its occurrence time is far
// in the future, the store state is unsafe, corrupt or missing while history
// exists, recorded history disappeared while the state still counts its
// events, or the write itself fails.
func (s *Store) Append(ctx context.Context, ev Event) (Event, error) {
	ctx = requestContext(ctx)
	cfg, err := s.config()
	if err != nil {
		return Event{}, err
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if ev, err = preflightEvent(ev, time.Now().UTC()); err != nil {
		return Event{}, err
	}
	if err := ensureStoreDir(cfg.dir); err != nil {
		return Event{}, err
	}
	lock, err := acquireLock(ctx, filepath.Join(cfg.dir, lockName))
	if err != nil {
		return Event{}, err
	}
	defer lock.release()
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	segments, state, stateExists, err := inspectStore(cfg.dir)
	if err != nil {
		return Event{}, err
	}
	now := time.Now().UTC()
	base, err := appendBase(ctx, cfg, segments, state, stateExists)
	if err != nil {
		return Event{}, err
	}
	assigned, line, err := buildAssignedEvent(ev, base.next, now)
	if err != nil {
		return Event{}, err
	}
	plan, err := planAppend(ctx, cfg, segments, base, int64(len(line)), now)
	if err != nil {
		return Event{}, err
	}
	// A cancelled append must not persist an event: recovery could not prove
	// the next sequence, and pruning would only observe the cancellation after
	// the line was already durable.
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	if !stateExists {
		// Publish the store identity and an empty counter document before the
		// first segment.
		if err := publishInitialState(cfg.dir, base.state); err != nil {
			return Event{}, err
		}
	}
	if err := writeSegmentLine(plan.path, line, plan.create); err != nil {
		_ = recordDropped(cfg.dir, base.state, true)
		return Event{}, fmt.Errorf("activity: append event: %w", err)
	}
	// The event is durable before anything is pruned. A failed or interrupted
	// append therefore never discards preexisting history.
	remaining, _, deleted, err := pruneRetention(ctx, cfg, plan.segments, plan.index, plan.cutoff, now)
	if err != nil {
		// The event is durable; only retention is incomplete. The next append
		// retries the budget.
		return Event{}, fmt.Errorf("activity: apply retention: %w", err)
	}
	if err := syncWrittenDir(cfg.dir, plan.create || deleted); err != nil {
		return Event{}, err
	}
	updated, err := updatedState(cfg, base, remaining, plan, assigned, now)
	if err != nil {
		return Event{}, err
	}
	if err := writeState(cfg.dir, updated); err != nil {
		// The event and the pruned history are durable but the counters are
		// not. Report the uncertainty; the next append rebuilds from the tail.
		return Event{}, fmt.Errorf("activity: record event state: %w", err)
	}
	return assigned, nil
}

// preflightEvent validates an event before the store is touched, so a rejected
// event creates no store.
func preflightEvent(ev Event, now time.Time) (Event, error) {
	ev = sanitizeEvent(ev)
	if err := ValidateEvent(ev); err != nil {
		return Event{}, err
	}
	occurred, err := normalizeEventTime(ev.OccurredAt, now)
	if err != nil {
		return Event{}, err
	}
	if err := checkOccurrenceSkew(occurred, now); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// inspectStore lists and safety-checks the segments and reads the state under
// the append lock. A store whose history and counters disagree is refused
// rather than rebaselined.
func inspectStore(dir string) ([]segmentInfo, storeState, bool, error) {
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, storeState{}, false, fmt.Errorf("activity: inspect store directory: %w", err)
	}
	if err := dirSafe(dirInfo); err != nil {
		return nil, storeState{}, false, err
	}
	segments, more, raced, err := listSegments(dir)
	if err != nil {
		return nil, storeState{}, false, err
	}
	if raced {
		// An external delete raced the enumeration. Re-list once for a
		// consistent view; the append lock already excludes our own writers.
		segments, more, _, err = listSegments(dir)
		if err != nil {
			return nil, storeState{}, false, err
		}
	}
	if more {
		return nil, storeState{}, false, errTooManySegments
	}
	if err := segmentSafeCheck(segments); err != nil {
		return nil, storeState{}, false, err
	}
	state, stateExists, err := readState(dir)
	if err != nil {
		return nil, storeState{}, false, err
	}
	if !stateExists && hasSegmentHistory(segments) {
		// The counters, including dropped-event history, cannot be rebuilt
		// from segments. Refuse rather than silently rebaseline them.
		return nil, storeState{}, false, errStateMissing
	}
	if stateExists && state.LastSequence > 0 && !hasSegmentHistory(segments) {
		// The state counts events but every segment holding them is gone.
		// Appending would rebaseline the counters and hide that loss from
		// reads, so the store refuses until a human inspects it.
		return nil, storeState{}, false, errHistoryMissing
	}
	return segments, state, stateExists, nil
}

// appendBasis is the resolved sequence basis for one append.
type appendBasis struct {
	state    storeState
	next     uint64
	tailAt   time.Time
	tailRead bool
}

// appendBase resolves the sequence of the next event. It trusts the state's
// sequence only when the active segment still has the size recorded with it.
// Otherwise the derived counters are rebuilt from the segment tail; a crash
// between the event write and the state write is thereby harmless and never
// reuses a sequence. A recovery that cannot prove the tail fails the append
// instead of falling back to a possibly stale state counter.
func appendBase(ctx context.Context, cfg storeConfig, segments []segmentInfo, state storeState, stateExists bool) (appendBasis, error) {
	var active segmentInfo
	hasActive := len(segments) > 0
	if hasActive {
		active = segments[len(segments)-1]
	}
	needTail := !stateExists
	if hasActive && stateExists && (state.ActiveSegment != active.index || state.ActiveBytes != active.size) {
		needTail = true
	}
	var (
		tailSeq  uint64
		tailAt   time.Time
		tailRead bool
	)
	if hasActive && needTail {
		last, ok, err := segmentLastEventErr(ctx, active, cfg.segmentBytes+maxEventBytes)
		if err != nil {
			// An interrupted recovery cannot prove the tail sequence. Treating
			// it as an absent tail would let this append reuse a sequence that
			// is already durable.
			return appendBasis{}, err
		}
		if ok {
			tailSeq = last.Sequence
			if t := eventTime(last); !t.IsZero() {
				tailAt = t
			}
		}
		tailRead = true
	}
	if tailSeq == ^uint64(0) {
		return appendBasis{}, errSequenceExhausted
	}
	if !stateExists {
		id, err := newStoreID()
		if err != nil {
			return appendBasis{}, err
		}
		state = storeState{Version: stateVersion, StoreID: id, NextSequence: 1}
	}
	next := state.NextSequence
	if tailSeq >= next {
		next = tailSeq + 1
	}
	if next == 0 {
		next = 1
	}
	if next == ^uint64(0) {
		return appendBasis{}, errSequenceExhausted
	}
	return appendBasis{state: state, next: next, tailAt: tailAt, tailRead: tailRead}, nil
}

// buildAssignedEvent applies the store's identity to ev and encodes its
// canonical line.
func buildAssignedEvent(ev Event, sequence uint64, now time.Time) (Event, []byte, error) {
	assigned := ev
	assigned.Schema = Schema
	id, err := newEventID()
	if err != nil {
		return Event{}, nil, err
	}
	assigned.EventID = id
	assigned.OccurredAt, err = normalizeEventTime(ev.OccurredAt, now)
	if err != nil {
		return Event{}, nil, err
	}
	if err := checkOccurrenceSkew(assigned.OccurredAt, now); err != nil {
		return Event{}, nil, err
	}
	assigned.Sequence = sequence
	if err := validateStoredEvent(assigned); err != nil {
		return Event{}, nil, err
	}
	line, err := encodeEvent(assigned)
	if err != nil {
		return Event{}, nil, err
	}
	return assigned, line, nil
}

// appendPlan is where one durable event line is written and what that write
// means for the segment set and the retained window.
type appendPlan struct {
	path     string
	index    uint64
	create   bool
	latest   string
	cutoff   time.Time
	segments []segmentInfo
}

// planAppend decides whether the append starts a new segment and folds the
// pending write into the listed segments.
func planAppend(ctx context.Context, cfg storeConfig, segments []segmentInfo, base appendBasis, lineBytes int64, now time.Time) (appendPlan, error) {
	var active segmentInfo
	hasActive := len(segments) > 0
	if hasActive {
		active = segments[len(segments)-1]
	}
	cleanEnd, err := segmentEndsWithNewline(active)
	if err != nil {
		return appendPlan{}, fmt.Errorf("activity: inspect active segment: %w", err)
	}
	latest := base.state.LastEventAt
	if base.tailRead {
		latest = laterString(latest, formatTime(base.tailAt))
	}
	cutoff := now.Add(-cfg.retention)
	rotate := !hasActive || !cleanEnd
	if !rotate && active.size > 0 {
		rotate = active.size+lineBytes > cfg.segmentBytes ||
			staleActive(ctx, cfg, active, latest, cutoff, now)
	}
	index, create, err := writeTarget(active, hasActive, rotate)
	if err != nil {
		return appendPlan{}, err
	}
	remaining, err := withWrittenSegment(cfg.dir, segments, index, create, lineBytes, now)
	if err != nil {
		return appendPlan{}, err
	}
	return appendPlan{
		path:     filepath.Join(cfg.dir, segmentFileName(index)),
		index:    index,
		create:   create,
		latest:   latest,
		cutoff:   cutoff,
		segments: remaining,
	}, nil
}

// staleActive reports whether the active segment is old enough that the append
// must start a new one. A stale state or an out-of-order tail may hide newer
// occurrences earlier in the active segment, so age-based rotation is proven by
// a bounded scan instead of trusting the tail alone.
func staleActive(ctx context.Context, cfg storeConfig, active segmentInfo, latest string, cutoff, now time.Time) bool {
	if latest == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, latest)
	if err != nil || t.After(now.Add(maxFutureSkew)) || !t.Before(cutoff) {
		return false
	}
	return segmentExpired(ctx, active, cfg.segmentBytes+maxEventBytes, cutoff, now)
}

// writeTarget is the segment index an append writes to and whether it must be
// created exclusively.
func writeTarget(active segmentInfo, hasActive, rotate bool) (uint64, bool, error) {
	if !hasActive {
		return 1, true, nil
	}
	if !rotate {
		return active.index, false, nil
	}
	if active.index == ^uint64(0) {
		return 0, false, errTooManySegments
	}
	return active.index + 1, true, nil
}

// withWrittenSegment is the segment listing after the planned write. It returns
// errCorruptState when an in-place write names a segment the listing lost.
func withWrittenSegment(dir string, segments []segmentInfo, index uint64, create bool, lineBytes int64, now time.Time) ([]segmentInfo, error) {
	remaining := append([]segmentInfo(nil), segments...)
	if create {
		name := segmentFileName(index)
		return append(remaining, segmentInfo{
			index:   index,
			name:    name,
			path:    filepath.Join(dir, name),
			size:    lineBytes,
			mode:    fileMode,
			modTime: now,
			ownerOK: true,
			regular: true,
		}), nil
	}
	for i := range remaining {
		if remaining[i].index == index {
			remaining[i].size += lineBytes
			remaining[i].modTime = now
			return remaining, nil
		}
	}
	return nil, errCorruptState
}

// updatedState is the counter document describing the store after the append
// and its retention pass.
func updatedState(cfg storeConfig, base appendBasis, remaining []segmentInfo, plan appendPlan, assigned Event, now time.Time) (storeState, error) {
	firstSeq, firstAt, firstOK := firstRetained(remaining, cfg)
	if !firstOK {
		firstSeq, firstAt = base.next, assigned.OccurredAt
	}
	lastAt := assigned.OccurredAt
	if !plan.create {
		// Keep the active segment's newest occurrence, not just its last
		// appended occurrence. Backdated events must not age fresh evidence.
		lastAt = laterString(plan.latest, lastAt)
	}
	writeSize, ok := segmentSize(remaining, plan.index)
	if !ok {
		return storeState{}, errCorruptState
	}
	updated := base.state
	updated.Version = stateVersion
	updated.NextSequence = base.next + 1
	updated.LastSequence = base.next
	updated.LastWriteAt = formatTime(now)
	updated.FirstSequence = firstSeq
	updated.FirstEventAt = firstAt
	updated.LastEventAt = lastAt
	updated.ActiveSegment = plan.index
	updated.ActiveBytes = writeSize
	if !updated.valid() {
		return storeState{}, errCorruptState
	}
	return updated, nil
}

// segmentSize is the recorded size of one segment in a listing.
func segmentSize(segments []segmentInfo, index uint64) (int64, bool) {
	for _, seg := range segments {
		if seg.index == index {
			return seg.size, true
		}
	}
	return 0, false
}

// publishInitialState writes the store identity and an empty counter document
// before the first segment. Readers then always see state before history and
// cannot mistake an in-flight first append for lost evidence.
func publishInitialState(dir string, state storeState) error {
	if err := writeState(dir, state); err != nil {
		return fmt.Errorf("activity: initialize event state: %w", err)
	}
	return nil
}

// syncWrittenDir flushes the directory entry when a segment was created or
// removed.
func syncWrittenDir(dir string, changed bool) error {
	if !changed {
		return nil
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("activity: sync store directory: %w", err)
	}
	return nil
}

// requestContext is the nil-safe context a public entry point runs under. A nil
// context carries no cancellation or deadline, so Background is its documented
// equivalent.
func requestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// pruneRetention removes the oldest segments after a successful append until
// the byte budget, the segment-count bound and the age bound are met. keep is
// the segment just written and is never removed.
func pruneRetention(ctx context.Context, cfg storeConfig, segments []segmentInfo, keep uint64, cutoff, now time.Time) ([]segmentInfo, int64, bool, error) {
	remaining := append([]segmentInfo(nil), segments...)
	total := int64(0)
	for _, seg := range remaining {
		total += seg.size
	}
	deleted := false
	for len(remaining) > 1 {
		candidate := remaining[0]
		if candidate.index == keep {
			break
		}
		overBytes := total > cfg.maxBytes
		overCount := len(remaining) > hardMaxSegments
		expired := false
		if !overBytes && !overCount {
			expired = segmentExpired(ctx, candidate, cfg.segmentBytes+maxEventBytes, cutoff, now)
		}
		if !overBytes && !overCount && !expired {
			break
		}
		if err := os.Remove(candidate.path); err != nil {
			return nil, total, deleted, fmt.Errorf("activity: remove expired segment: %w", err)
		}
		total -= candidate.size
		remaining = remaining[1:]
		deleted = true
	}
	return remaining, total, deleted, nil
}

// segmentExpired uses the tail as a cheap proof that a segment is still fresh.
// An old tail is not proof of expiry: occurrence times need not be monotonic.
func segmentExpired(ctx context.Context, seg segmentInfo, budget int64, cutoff, now time.Time) bool {
	if last, ok := segmentLastEvent(ctx, seg, budget); ok {
		t := eventTime(last)
		if !t.IsZero() && !t.After(now.Add(maxFutureSkew)) && !t.Before(cutoff) {
			return false
		}
	}
	t, ok := retentionTime(ctx, seg, budget, now)
	return ok && t.Before(cutoff)
}

// retentionTime is the newest plausible occurrence anywhere in a segment, or
// its file time when no plausible timestamps exist. An incomplete scan cannot
// prove expiry and never authorizes age-based deletion.
func retentionTime(ctx context.Context, seg segmentInfo, budget int64, now time.Time) (time.Time, bool) {
	if budget <= 0 || seg.size > budget {
		return time.Time{}, false
	}
	f, err := openSegmentRead(seg)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	var newest time.Time
	_, err = walkSegment(ctx, f, seg.size, false, false, 0, func(ev Event) bool {
		t := eventTime(ev)
		if !t.After(now.Add(maxFutureSkew)) && t.After(newest) {
			newest = t
		}
		return true
	})
	if err != nil {
		return time.Time{}, false
	}
	if !newest.IsZero() {
		return newest, true
	}
	if !seg.modTime.IsZero() && !seg.modTime.After(now.Add(maxFutureSkew)) {
		return seg.modTime, true
	}
	return time.Time{}, false
}

// writeSegmentLine is the append seam so tests can exercise a failed write
// without a fault filesystem.
var writeSegmentLine = appendSegmentLine

// appendSegmentLine writes one complete line and syncs it. A new segment is
// created exclusively so an existing segment's content is never overwritten.
func appendSegmentLine(path string, line []byte, create bool) (err error) {
	flags := os.O_WRONLY | noFollowFlag
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	} else {
		flags |= os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, fileMode)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if err := verifyOpenSegment(f); err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}

// firstRetained returns the first event of the oldest remaining segment.
func firstRetained(segments []segmentInfo, cfg storeConfig) (uint64, string, bool) {
	if len(segments) == 0 {
		return 0, "", false
	}
	ev, ok := segmentFirstEvent(segments[0], cfg.segmentBytes+maxEventBytes)
	if !ok {
		return 0, "", false
	}
	return ev.Sequence, ev.OccurredAt, true
}

// recordDropped counts an event the store failed to persist. It is best effort:
// the append error itself is always returned.
func recordDropped(dir string, state storeState, exists bool) error {
	if !exists || !state.valid() {
		return nil
	}
	state.DroppedEvents++
	return writeState(dir, state)
}

// storeIDPattern is the private store identity persisted in state.json and
// bound into every cursor.
var storeIDPattern = regexp.MustCompile(`^st-[0-9a-f]{32}$`)

// validStoreID reports whether id is one persisted store identity.
func validStoreID(id string) bool {
	return storeIDPattern.MatchString(id)
}

// newStoreID returns a random store identity.
func newStoreID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("activity: generate store id: %w", err)
	}
	return "st-" + hex.EncodeToString(raw[:]), nil
}

// newEventID returns a random, unique event identity.
func newEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("activity: generate event id: %w", err)
	}
	return "evt-" + hex.EncodeToString(raw[:]), nil
}

// ensureStoreDir creates the private activity directory if it is missing. The
// shared parent may be 0750; only the new private directory is forced to 0700.
// An existing directory is checked, never repaired.
func ensureStoreDir(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, parentMode); err != nil {
		return fmt.Errorf("activity: create store parent: %w", err)
	}
	if err := os.Mkdir(dir, dirMode); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("activity: create store directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("activity: inspect store directory: %w", err)
	}
	return dirSafe(info)
}

// dirSafe refuses a directory that is a symlink, not a directory, not owned by
// this user, or not exactly 0700.
func dirSafe(info os.FileInfo) error {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errUnsafeDir
	}
	if info.Mode().Perm() != dirMode {
		return errUnsafeDir
	}
	if !ownerMatches(info) {
		return errUnsafeDir
	}
	return nil
}
