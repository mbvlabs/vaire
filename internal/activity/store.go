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
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := s.config()
	if err != nil {
		return Event{}, err
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	ev = sanitizeEvent(ev)
	if err := ValidateEvent(ev); err != nil {
		return Event{}, err
	}
	// Validate the occurrence time against the local clock before touching the
	// filesystem, so a rejected event creates no store.
	preflightNow := time.Now().UTC()
	preflightOccurred, err := normalizeEventTime(ev.OccurredAt, preflightNow)
	if err != nil {
		return Event{}, err
	}
	if err := checkOccurrenceSkew(preflightOccurred, preflightNow); err != nil {
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
	dirInfo, err := os.Lstat(cfg.dir) //nolint:gosec // G304: the store directory.
	if err != nil {
		return Event{}, fmt.Errorf("activity: inspect store directory: %w", err)
	}
	if err := dirSafe(dirInfo); err != nil {
		return Event{}, err
	}
	segments, more, raced, err := listSegments(cfg.dir)
	if err != nil {
		return Event{}, err
	}
	if raced {
		// An external delete raced the enumeration. Re-list once for a
		// consistent view; the append lock already excludes our own writers.
		segments, more, _, err = listSegments(cfg.dir)
		if err != nil {
			return Event{}, err
		}
	}
	if more {
		return Event{}, errTooManySegments
	}
	if err := segmentSafeCheck(segments); err != nil {
		return Event{}, err
	}
	state, stateExists, err := readState(cfg.dir)
	if err != nil {
		return Event{}, err
	}
	hasHistory := false
	for _, seg := range segments {
		if seg.size > 0 {
			hasHistory = true
			break
		}
	}
	if !stateExists && hasHistory {
		// The counters, including dropped-event history, cannot be rebuilt
		// from segments. Refuse rather than silently rebaseline them.
		return Event{}, errStateMissing
	}
	if stateExists && state.LastSequence > 0 && !hasHistory {
		// The state counts events but every segment holding them is gone.
		// Appending would rebaseline the counters and hide that loss from
		// reads, so the store refuses until a human inspects it.
		return Event{}, errHistoryMissing
	}
	now := time.Now().UTC()
	active, hasActive := segmentInfo{}, false
	if len(segments) > 0 {
		active, hasActive = segments[len(segments)-1], true
	}
	// Trust the state's sequence only when the active segment still has the
	// size recorded with it. Otherwise rebuild the derived counters from the
	// segment tail; a crash between the event write and the state write is
	// thereby harmless and never reuses a sequence.
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
		if last, ok := segmentLastEvent(active, cfg.segmentBytes+maxEventBytes); ok {
			tailSeq = last.Sequence
			if t := eventTime(last); !t.IsZero() {
				tailAt = t
			}
		}
		tailRead = true
	}
	if tailSeq == ^uint64(0) {
		return Event{}, errSequenceExhausted
	}
	if !stateExists {
		id, idErr := newStoreID()
		if idErr != nil {
			return Event{}, idErr
		}
		state = storeState{Version: stateVersion, StoreID: id, NextSequence: 1}
	}
	next := state.NextSequence
	if tailSeq >= next {
		if tailSeq == ^uint64(0) {
			return Event{}, errSequenceExhausted
		}
		next = tailSeq + 1
	}
	if next == 0 {
		next = 1
	}
	if next == ^uint64(0) {
		return Event{}, errSequenceExhausted
	}
	assigned := ev
	assigned.Schema = Schema
	assigned.EventID, err = newEventID()
	if err != nil {
		return Event{}, err
	}
	assigned.OccurredAt, err = normalizeEventTime(ev.OccurredAt, now)
	if err != nil {
		return Event{}, err
	}
	if err := checkOccurrenceSkew(assigned.OccurredAt, now); err != nil {
		return Event{}, err
	}
	assigned.Sequence = next
	if err := validateStoredEvent(assigned); err != nil {
		return Event{}, err
	}
	line, err := encodeEvent(assigned)
	if err != nil {
		return Event{}, err
	}
	cleanEnd, err := segmentEndsWithNewline(active)
	if err != nil {
		return Event{}, fmt.Errorf("activity: inspect active segment: %w", err)
	}
	latest := state.LastEventAt
	if tailRead {
		latest = laterString(latest, formatTime(tailAt))
	}
	cutoff := now.Add(-cfg.retention)
	rotate := !hasActive || !cleanEnd
	if hasActive && active.size > 0 && !rotate {
		if active.size+int64(len(line)) > cfg.segmentBytes {
			rotate = true
		} else if latest != "" {
			if t, parseErr := time.Parse(time.RFC3339Nano, latest); parseErr == nil &&
				!t.After(now.Add(maxFutureSkew)) && t.Before(cutoff) {
				// A stale state or an out-of-order tail may hide newer
				// occurrences earlier in the active segment.
				rotate = segmentExpired(ctx, active, cfg.segmentBytes+maxEventBytes, cutoff, now)
			}
		}
	}
	writeIndex := uint64(1)
	create := true
	if hasActive {
		writeIndex = active.index
		create = false
		if rotate {
			if active.index == ^uint64(0) {
				return Event{}, errTooManySegments
			}
			writeIndex = active.index + 1
			create = true
		}
	}
	writePath := filepath.Join(cfg.dir, segmentFileName(writeIndex))
	if !stateExists {
		// Publish the store identity and an empty counter document before the
		// first segment. Readers then always see state before history and
		// cannot mistake an in-flight first append for lost evidence.
		if err := writeState(cfg.dir, state); err != nil {
			return Event{}, fmt.Errorf("activity: initialize event state: %w", err)
		}
		stateExists = true
	}
	if err := writeSegmentLine(writePath, line, create); err != nil {
		_ = recordDropped(cfg.dir, state, stateExists)
		return Event{}, fmt.Errorf("activity: append event: %w", err)
	}
	// The event is durable before anything is pruned. A failed or interrupted
	// append therefore never discards preexisting history.
	remaining := append([]segmentInfo(nil), segments...)
	if create {
		remaining = append(remaining, segmentInfo{
			index:   writeIndex,
			name:    segmentFileName(writeIndex),
			path:    writePath,
			size:    int64(len(line)),
			mode:    fileMode,
			modTime: now,
			ownerOK: true,
			regular: true,
		})
	} else {
		found := false
		for i := range remaining {
			if remaining[i].index == writeIndex {
				remaining[i].size += int64(len(line))
				remaining[i].modTime = now
				found = true
				break
			}
		}
		if !found {
			return Event{}, errCorruptState
		}
	}
	remaining, _, deleted, err := pruneRetention(ctx, cfg, remaining, writeIndex, cutoff, now)
	if err != nil {
		// The event is durable; only retention is incomplete. The next append
		// retries the budget.
		return Event{}, fmt.Errorf("activity: apply retention: %w", err)
	}
	if create || deleted {
		if err := syncDir(cfg.dir); err != nil {
			return Event{}, fmt.Errorf("activity: sync store directory: %w", err)
		}
	}
	firstSeq, firstAt, firstOK := firstRetained(remaining, cfg)
	if !firstOK {
		firstSeq, firstAt = next, assigned.OccurredAt
	}
	lastAt := assigned.OccurredAt
	if !create {
		// Keep the active segment's newest occurrence, not just its last
		// appended occurrence. Backdated events must not age fresh evidence.
		lastAt = laterString(latest, lastAt)
	}
	writeSize := int64(-1)
	for _, seg := range remaining {
		if seg.index == writeIndex {
			writeSize = seg.size
			break
		}
	}
	if writeSize < 0 {
		return Event{}, errCorruptState
	}
	updated := state
	updated.Version = stateVersion
	updated.NextSequence = next + 1
	updated.LastSequence = next
	updated.LastWriteAt = formatTime(now)
	updated.FirstSequence = firstSeq
	updated.FirstEventAt = firstAt
	updated.LastEventAt = lastAt
	updated.ActiveSegment = writeIndex
	updated.ActiveBytes = writeSize
	if !updated.valid() {
		return Event{}, errCorruptState
	}
	if err := writeState(cfg.dir, updated); err != nil {
		// The event and the pruned history are durable but the counters are
		// not. Report the uncertainty; the next append rebuilds from the tail.
		return Event{}, fmt.Errorf("activity: record event state: %w", err)
	}
	return assigned, nil
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
		if err := os.Remove(candidate.path); err != nil { //nolint:gosec // G304: a listed store segment.
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
	if last, ok := segmentLastEvent(seg, budget); ok {
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
	f, err := os.OpenFile(path, flags, fileMode) //nolint:gosec // G304: a path inside the store directory.
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
	info, err := os.Lstat(dir) //nolint:gosec // G304: the store directory.
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
