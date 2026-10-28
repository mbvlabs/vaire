package activity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"
)

// afterStateReadHook, beforeStateRecheckHook and afterListSegmentsHook are
// test seams. They reproduce a writer completing between two observations
// deterministically.
var (
	afterStateReadHook     func()
	beforeStateRecheckHook func()
	afterListSegmentsHook  func()
)

// maxSnapshotAttempts bounds how many times a read re-observes state and
// segments when retention removes a listed segment before the retained bounds
// are read. A normal rotation invalidates only the listing it raced, so the
// next observation succeeds; the bound keeps a read that races continuous
// rotation finite.
const maxSnapshotAttempts = 4

// storeSnapshot is one observation of the store's state, segment listing and
// retained bounds. A vanished segment means the listing and the state describe
// different snapshots, so a bound comparison against state is not proof of
// lost evidence.
type storeSnapshot struct {
	state        storeState
	stateExists  bool
	stateErr     error
	segments     []segmentInfo
	moreEntries  bool
	racedEntries bool
	ret          retainedBounds
	vanished     bool
}

// observeStore reads the store state and segment listing until the retained
// bounds are derived from the same segment set that was listed. Every attempt
// reads state before listing because a writer publishes a segment before its
// state: a state observed first can never claim an event that a later listing
// lacks. Retention can still remove a listed segment before its bound is read;
// re-listing only that segment set would pair a stale state with a newer
// history, so the whole observation restarts. The last observation is returned
// when the bound is exhausted, with vanished set so callers never infer loss
// from a mixed snapshot.
func observeStore(ctx context.Context, dir string, cfg storeConfig, checkSafe bool) (storeSnapshot, error) {
	var snap storeSnapshot
	for attempt := 0; attempt < maxSnapshotAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return storeSnapshot{}, err
		}
		observed, err := observeStoreOnce(dir, cfg, checkSafe)
		if err != nil {
			return storeSnapshot{}, err
		}
		snap = observed
		if !snap.vanished {
			break
		}
	}
	return snap, nil
}

// observeStoreOnce is one state-then-listing observation, including the
// recheck that distinguishes a first append from a genuinely missing state.
func observeStoreOnce(dir string, cfg storeConfig, checkSafe bool) (storeSnapshot, error) {
	state, stateExists, stateErr := readState(dir)
	if afterStateReadHook != nil {
		afterStateReadHook()
	}
	segments, moreEntries, racedEntries, err := listSegments(dir)
	if err != nil {
		return storeSnapshot{}, err
	}
	if afterListSegmentsHook != nil {
		afterListSegmentsHook()
	}
	if checkSafe {
		if err := segmentSafeCheck(segments); err != nil {
			return storeSnapshot{}, err
		}
	}
	if !stateExists && hasSegmentHistory(segments) {
		if beforeStateRecheckHook != nil {
			beforeStateRecheckHook()
		}
		restored, restoredOK, recheckErr := readState(dir)
		switch {
		case recheckErr == nil && restoredOK:
			state, stateExists = restored, true
			// Re-list after the recheck so the same state-before-listing
			// ordering still holds; otherwise a newer append could make the
			// rechecked state look ahead of the first listing.
			segments, moreEntries, racedEntries, err = listSegments(dir)
			if err != nil {
				return storeSnapshot{}, err
			}
			if checkSafe {
				if err := segmentSafeCheck(segments); err != nil {
					return storeSnapshot{}, err
				}
			}
		case recheckErr == nil:
		default:
			stateErr = recheckErr
		}
	}
	ret, vanished := readRetainedBounds(segments, cfg)
	return storeSnapshot{
		state:        state,
		stateExists:  stateExists,
		stateErr:     stateErr,
		segments:     segments,
		moreEntries:  moreEntries,
		racedEntries: racedEntries,
		ret:          ret,
		vanished:     vanished,
	}, nil
}

// Query returns one bounded page of retained events in ascending sequence
// order. A missing store directory is reported as unavailable evidence, not as
// an empty healthy history. An unsafe directory or segment is refused with an
// error. Query never creates, repairs or modifies anything.
//
// A cursor is bound to the store instance, the filters, the frozen time window
// and the limit of the page it came from: changing any of them is an error, not
// a new query. It also carries the first page's sequence highwater, so appends
// cannot duplicate or reorder a page chain. A cursor whose evidence retention
// removed (or whose state cannot verify the store instance) reports
// Coverage.CursorExpired and no events; it never restarts silently.
//
// A page may read one whole segment to guarantee forward progress; the absolute
// page cap still bounds it. If a single segment exceeds that cap and the page
// cannot advance past the cursor, the page reports Truncated and Partial with
// no NextCursor instead of repeating the same cursor forever. A cursor carried
// to a recreated store (new store identity) is a cursor mismatch error, not a
// continuation.
//
// A missing or corrupt state file with retained history is reported as lost
// evidence: the events are still read, but the counters and store identity are
// unknown, so Partial and LossDetected are set. Because a continuation cannot
// be bound to a verified store instance, such a query returns at most one page
// and no NextCursor. Recovering the missing state is an operator action; this
// package never rebuilds or heals it.
func (s *Store) Query(ctx context.Context, q Query) (Page, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := s.config()
	if err != nil {
		return Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	nq, err := normalizeQuery(q, time.Now().UTC())
	if err != nil {
		return Page{}, err
	}
	page := Page{Schema: Schema, Events: []Event{}}
	var cov Coverage
	info, err := os.Lstat(cfg.dir) //nolint:gosec // G304: the store directory.
	if err != nil {
		if isNotExist(err) {
			cov.Available = false
			cov.Partial = true
			page.Coverage = cov
			return page, nil
		}
		return Page{}, fmt.Errorf("activity: inspect store directory: %w", err)
	}
	if err := dirSafe(info); err != nil {
		return Page{}, err
	}
	cov.Available = true
	// Read state before listing segments. A writer persists a segment before
	// writing its state, so a state observed first can never claim an event
	// that the later listing does not contain; the reverse order produced
	// false "evidence missing" verdicts on healthy concurrent appends. A
	// segment that retention removes while its bound is read restarts the
	// whole observation; see observeStore.
	snap, err := observeStore(ctx, cfg.dir, cfg, true)
	if err != nil {
		return Page{}, err
	}
	state, stateExists := snap.state, snap.stateExists
	segments, moreEntries, racedEntries := snap.segments, snap.moreEntries, snap.racedEntries
	stateCorrupt := errors.Is(snap.stateErr, errCorruptState)
	if snap.stateErr != nil && !stateCorrupt {
		return Page{}, snap.stateErr
	}
	ret := snap.ret
	// A listed segment that vanished during the bound read means state and
	// bounds come from different snapshots. The state may still name an event
	// that the fresh listing holds, so the stale bound is not proof that
	// evidence is missing. observeStore has already retried; a remaining
	// inconsistency makes the read incomplete, never a loss verdict.
	evidenceMissing := !snap.vanished && stateExists && state.LastSequence > 0 &&
		(!ret.hasLast || ret.lastSeq < state.LastSequence)
	stateLost := stateCorrupt || evidenceMissing
	if !stateExists && hasSegmentHistory(segments) {
		stateLost = true
	}
	storeID := ""
	if stateExists {
		storeID = state.StoreID
	}
	high := highwater(state, stateExists, ret)
	cov.FirstRetainedAt = retainedFirstString(ret)
	cov.LastRetainedAt = retainedLastString(ret)
	if stateExists {
		cov.DroppedEvents = state.DroppedEvents
	}
	after := uint64(0)
	snapshot := high
	if nq.cur != nil {
		after = nq.cur.After
		snapshot = nq.cur.Snapshot
		if !stateExists {
			// Without state the store instance and the loss counters cannot be
			// verified, and the cursor cannot be trusted to continue safely.
			cov.CursorExpired = true
			cov.Partial = true
			cov.LossDetected = true
			page.Coverage = cov
			return page, nil
		}
		if nq.cur.StoreID != storeID {
			return Page{}, errCursorMismatch
		}
		if cursorExpired(nq.cur, ret, high, len(segments)) {
			cov.CursorExpired = true
			cov.Partial = true
			cov.LossDetected = stateLost || cov.DroppedEvents > 0
			page.Coverage = cov
			return page, nil
		}
	}
	budget := scanBudget(cfg)
	var (
		matches    []Event
		scanned    segmentWalk
		truncated  = moreEntries || racedEntries || snap.vanished
		guaranteed bool
	)
	for i, seg := range segments {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		if seg.size == 0 {
			continue
		}
		if guaranteed && scanned.bytes >= budget {
			truncated = true
			break
		}
		if after > 0 {
			// A segment whose last sequence is at or before the cursor cannot
			// contribute a returned event. Skip its content without decoding
			// it, but keep the sequence baseline so later gap counts stay
			// honest for the scanned range.
			if last, ok := segmentLastEvent(seg, cfg.segmentBytes+maxEventBytes); ok && last.Sequence <= after {
				if last.Sequence > scanned.prevSeq {
					scanned.prevSeq = last.Sequence
				}
				continue
			}
		}
		limit := seg.size
		cutAtLimit := false
		if !guaranteed {
			// Read one whole segment (up to the absolute page cap) so a page
			// always makes progress instead of re-reading the same budget
			// prefix behind the cursor forever.
			if limit > maxPageBytes {
				limit = maxPageBytes
				cutAtLimit = true
				truncated = true
			}
			guaranteed = true
		} else {
			remaining := budget - scanned.bytes
			if remaining <= 0 {
				truncated = true
				break
			}
			if remaining < limit {
				limit = remaining
				cutAtLimit = true
				truncated = true
			}
		}
		f, openErr := openSegmentRead(seg)
		if openErr != nil {
			if errors.Is(openErr, errUnsafeSegment) {
				return Page{}, openErr
			}
			if errors.Is(openErr, fs.ErrNotExist) {
				// Retention removed the segment after it was listed.
				truncated = true
				continue
			}
			scanned.malformedSegments++
			if cutAtLimit {
				truncated = true
				break
			}
			continue
		}
		walk, walkErr := walkSegment(ctx, f, limit, cutAtLimit, i == len(segments)-1, scanned.prevSeq, func(ev Event) bool {
			if ev.Sequence <= after || ev.Sequence > snapshot {
				return true
			}
			if !matchesQuery(nq, ev) {
				return true
			}
			matches = append(matches, ev)
			return len(matches) <= nq.limit
		})
		_ = f.Close()
		if walkErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Page{}, ctxErr
			}
			scanned.malformedSegments++
			if cutAtLimit {
				truncated = true
				break
			}
			continue
		}
		walk.merge(&scanned)
		scanned.segments++
		if walk.inFlightTail {
			// A writer is still appending the newest record: this page is a
			// prefix of the retained evidence, not a complete scan of it.
			truncated = true
		}
		if walk.stoppedByVisit {
			break
		}
		if cutAtLimit {
			break
		}
	}
	if !truncated && !scanned.stoppedByVisit && scanned.hasFirst {
		// A complete scan gives exact retained time bounds even when supplied
		// occurrence timestamps are not monotonic with sequence order.
		ret.firstSeq, ret.firstAt, ret.hasFirst = scanned.first.Sequence, scanned.earliest, true
		ret.lastSeq, ret.lastAt, ret.hasLast = scanned.last.Sequence, scanned.latest, true
		cov.FirstRetainedAt = retainedFirstString(ret)
		cov.LastRetainedAt = retainedLastString(ret)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Sequence < matches[j].Sequence })
	more := scanned.stoppedByVisit || len(matches) > nq.limit
	returnedAll := len(matches) <= nq.limit
	if len(matches) > nq.limit {
		matches = matches[:nq.limit]
	}
	page.Events = matches
	cov.EventsReturned = len(matches)
	cov.EventsScanned = scanned.events
	cov.SegmentsScanned = scanned.segments
	cov.BytesScanned = scanned.bytes
	cov.MalformedSegments = scanned.malformedSegments
	cov.MalformedEvents = scanned.malformedEvents
	cov.SequenceGaps = scanned.gaps
	cov.Truncated = truncated
	cov.LimitReached = len(matches) == nq.limit && nq.limit > 0
	if len(matches) > 0 {
		first, last := matches[0].OccurredAt, matches[0].OccurredAt
		for _, ev := range matches[1:] {
			first = earlierString(first, ev.OccurredAt)
			last = laterString(last, ev.OccurredAt)
		}
		cov.FirstReturnedAt = first
		cov.LastReturnedAt = last
	}
	boundedWithoutProgress := false
	if more || truncated {
		afterSeq := uint64(0)
		if len(matches) > 0 {
			afterSeq = matches[len(matches)-1].Sequence
		}
		if truncated && returnedAll && scanned.lastScannedSeq > afterSeq {
			afterSeq = scanned.lastScannedSeq
		}
		if afterSeq > after {
			page.NextCursor = cursorFor(nq, afterSeq, snapshot, storeID)
		} else {
			// The page could not advance past the cursor. Never repeat it:
			// report a bounded, incomplete read with no continuation.
			boundedWithoutProgress = true
		}
	}
	cov.LossDetected = stateLost || cov.DroppedEvents > 0 ||
		cov.MalformedSegments > 0 || cov.MalformedEvents > 0 || cov.SequenceGaps > 0
	cov.Partial = cov.LossDetected || snap.vanished || boundedWithoutProgress ||
		windowBeforeRetained(nq, ret, len(segments))
	page.Coverage = cov
	return page, nil
}

// hasSegmentHistory reports whether any segment carries bytes, which means the
// state file is required to describe it honestly.
func hasSegmentHistory(segments []segmentInfo) bool {
	for _, seg := range segments {
		if seg.size > 0 {
			return true
		}
	}
	return false
}

// retainedBounds is the oldest and newest retained evidence derived from the
// actual segments.
type retainedBounds struct {
	firstSeq uint64
	firstAt  time.Time
	hasFirst bool
	lastSeq  uint64
	lastAt   time.Time
	hasLast  bool
}

// readRetainedBounds derives the retained window only from the oldest
// decodable event of the oldest segments and the newest decodable event of the
// newest segments. The state file is never used here, so a stale state can
// never widen coverage or claim evidence that segments no longer hold. vanished
// reports that a listed segment was removed before its edge could be read: the
// listing and the bounds then belong to different snapshots and a missing last
// sequence cannot be inferred from them.
func readRetainedBounds(segments []segmentInfo, cfg storeConfig) (retainedBounds, bool) {
	var ret retainedBounds
	vanished := false
	edge := cfg.segmentBytes + maxEventBytes
	budget := scanBudget(cfg)
	for _, seg := range segments {
		if seg.size <= 0 {
			continue
		}
		if budget <= 0 {
			break
		}
		segmentBudget := edge
		if segmentBudget > budget {
			segmentBudget = budget
		}
		ev, ok, err := segmentFirstEventErr(seg, segmentBudget)
		if isNotExist(err) {
			vanished = true
		}
		if ok {
			ret.firstSeq, ret.firstAt, ret.hasFirst = ev.Sequence, eventTime(ev), true
			break
		}
		budget -= seg.size
	}
	budget = scanBudget(cfg)
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if seg.size <= 0 {
			continue
		}
		if budget <= 0 {
			break
		}
		segmentBudget := edge
		if segmentBudget > budget {
			segmentBudget = budget
		}
		ev, ok, err := segmentLastEventErr(seg, segmentBudget)
		if isNotExist(err) {
			vanished = true
		}
		if ok {
			ret.lastSeq, ret.lastAt, ret.hasLast = ev.Sequence, eventTime(ev), true
			break
		}
		budget -= seg.size
	}
	if ret.hasFirst && ret.hasLast && ret.lastAt.Before(ret.firstAt) {
		ret.lastAt = ret.firstAt
	}
	return ret, vanished
}

// highwater is the newest sequence the store may have retained.
func highwater(state storeState, stateExists bool, ret retainedBounds) uint64 {
	high := ret.lastSeq
	if stateExists && state.LastSequence > high {
		high = state.LastSequence
	}
	return high
}

// cursorExpired reports whether the cursor points before evidence that
// retention has removed. It never resets such a cursor to the beginning.
func cursorExpired(cur *cursorState, ret retainedBounds, high uint64, segmentCount int) bool {
	if high == 0 && cur.After > 0 {
		return true
	}
	if cur.After > high {
		return true
	}
	if cur.After == 0 {
		return false
	}
	if segmentCount == 0 {
		return true
	}
	if ret.hasFirst && ret.firstSeq > cur.After+1 {
		return true
	}
	return false
}

// windowBeforeRetained reports whether the requested time window reaches
// before evidence that retention actually removed. A window before the first
// event of an intact history is simply empty, not lost.
func windowBeforeRetained(nq normalizedQuery, ret retainedBounds, segmentCount int) bool {
	if !ret.hasFirst {
		return segmentCount > 0
	}
	if nq.since.IsZero() {
		if ret.firstSeq > 1 {
			return true
		}
	} else if ret.firstSeq > 1 && nq.since.Before(ret.firstAt) {
		return true
	}
	if !nq.until.IsZero() && !nq.until.After(ret.firstAt) {
		return ret.firstSeq > 1
	}
	return false
}

// matchesQuery applies the exact filters and the inclusive/exclusive time
// window.
func matchesQuery(nq normalizedQuery, ev Event) bool {
	q := nq.filter
	if q.OperationID != "" && ev.OperationID != q.OperationID {
		return false
	}
	if q.RequestID != "" && ev.RequestID != q.RequestID {
		return false
	}
	if q.JobID != "" && ev.JobID != q.JobID {
		return false
	}
	if q.Database != "" && ev.Database != q.Database {
		return false
	}
	if q.Action != "" && ev.Action != q.Action {
		return false
	}
	if q.Outcome != "" && ev.Outcome != q.Outcome {
		return false
	}
	if nq.since.IsZero() && nq.until.IsZero() {
		return true
	}
	t := eventTime(ev)
	if t.IsZero() {
		return false
	}
	if !nq.since.IsZero() && t.Before(nq.since) {
		return false
	}
	if !nq.until.IsZero() && !t.Before(nq.until) {
		return false
	}
	return true
}

func retainedFirstString(ret retainedBounds) string {
	if !ret.hasFirst {
		return ""
	}
	return formatTime(ret.firstAt)
}

func retainedLastString(ret retainedBounds) string {
	if !ret.hasLast {
		return ""
	}
	return formatTime(ret.lastAt)
}
