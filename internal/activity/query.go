package activity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
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

	for range maxSnapshotAttempts {
		if err := ctx.Err(); err != nil {
			return storeSnapshot{}, err
		}

		observed, err := observeStoreOnce(ctx, dir, cfg, checkSafe)
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
func observeStoreOnce(ctx context.Context, dir string, cfg storeConfig, checkSafe bool) (storeSnapshot, error) {
	state, stateExists, stateErr := readState(dir)

	if afterStateReadHook != nil {
		afterStateReadHook()
	}

	listing, err := readListing(dir, checkSafe)
	if err != nil {
		return storeSnapshot{}, err
	}

	if afterListSegmentsHook != nil {
		afterListSegmentsHook()
	}

	if !stateExists && hasSegmentHistory(listing.segments) {
		recheck, err := restoreMissingState(dir, checkSafe, stateErr, listing)
		if err != nil {
			return storeSnapshot{}, err
		}

		state, stateExists, stateErr, listing = recheck.state, recheck.stateExists, recheck.stateErr, recheck.listing
	}

	ret, vanished := readRetainedBounds(ctx, listing.segments, cfg)

	return storeSnapshot{
		state:        state,
		stateExists:  stateExists,
		stateErr:     stateErr,
		segments:     listing.segments,
		moreEntries:  listing.moreEntries,
		racedEntries: listing.racedEntries,
		ret:          ret,
		vanished:     vanished,
	}, nil
}

// storeListing is one directory listing plus the safety and bound verdicts that
// travel with it.
type storeListing struct {
	segments     []segmentInfo
	moreEntries  bool
	racedEntries bool
}

// readListing lists the segments and applies the optional safety refusal.
func readListing(dir string, checkSafe bool) (storeListing, error) {
	segments, more, raced, err := listSegments(dir)
	if err != nil {
		return storeListing{}, err
	}

	if checkSafe {
		if err := segmentSafeCheck(segments); err != nil {
			return storeListing{}, err
		}
	}

	return storeListing{segments: segments, moreEntries: more, racedEntries: raced}, nil
}

// stateRecheck is the outcome of re-reading state that the first observation
// found missing while history existed.
type stateRecheck struct {
	state       storeState
	stateExists bool
	stateErr    error
	listing     storeListing
}

// restoreMissingState distinguishes a first append from genuinely missing
// state. State is re-read once; a state that appeared is paired with a fresh
// listing so the same state-before-listing ordering still holds, otherwise a
// newer append could make the rechecked state look ahead of the first listing.
func restoreMissingState(dir string, checkSafe bool, stateErr error, listing storeListing) (stateRecheck, error) {
	if beforeStateRecheckHook != nil {
		beforeStateRecheckHook()
	}

	restored, restoredOK, recheckErr := readState(dir)
	switch {
	case recheckErr == nil && restoredOK:
		fresh, err := readListing(dir, checkSafe)
		if err != nil {
			return stateRecheck{}, err
		}

		return stateRecheck{state: restored, stateExists: true, listing: fresh}, nil
	case recheckErr == nil:
		return stateRecheck{stateErr: stateErr, listing: listing}, nil
	default:
		return stateRecheck{stateErr: recheckErr, listing: listing}, nil
	}
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
	ctx = requestContext(ctx)

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

	snap, available, err := loadSnapshot(ctx, cfg)
	if err != nil {
		return Page{}, err
	}

	if !available {
		page.Coverage = Coverage{Available: false, Partial: true}
		return page, nil
	}

	facts, err := deriveFacts(snap)
	if err != nil {
		return Page{}, err
	}

	cov := baseCoverage(facts)

	after, snapshot, expired, err := cursorPosition(nq, facts, cov)
	if err != nil {
		return Page{}, err
	}

	if expired != nil {
		page.Coverage = *expired
		return page, nil
	}

	ret := facts.ret

	scan := &pageScan{
		nq:        nq,
		cfg:       cfg,
		budget:    scanBudget(cfg),
		after:     after,
		snapshot:  snapshot,
		segments:  facts.segments,
		truncated: snap.moreEntries || snap.racedEntries || snap.vanished,
	}
	if err := scan.run(ctx); err != nil {
		return Page{}, err
	}

	if !scan.truncated && !scan.walk.stoppedByVisit && scan.walk.hasFirst {
		// A complete scan gives exact retained time bounds even when supplied
		// occurrence timestamps are not monotonic with sequence order.
		ret = exactRetainedBounds(scan.walk)
		cov.FirstRetainedAt = retainedFirstString(ret)
		cov.LastRetainedAt = retainedLastString(ret)
	}

	sort.Slice(scan.matches, func(i, j int) bool { return scan.matches[i].Sequence < scan.matches[j].Sequence })
	returnedAll := len(scan.matches) <= nq.limit

	more := scan.walk.stoppedByVisit || !returnedAll
	if !returnedAll {
		scan.matches = scan.matches[:nq.limit]
	}

	page.Events = scan.matches
	coverageFromScan(&cov, nq, scan)
	next, stalled := continuation(nq, scan, after, snapshot, facts.storeID, more, returnedAll)
	page.NextCursor = next

	finishVerdict(&cov, nq, snap, ret, stalled, len(facts.segments), facts.stateLost)
	page.Coverage = cov

	return page, nil
}

// loadSnapshot reads one consistent observation of the store. available is
// false when the store directory does not exist, which is unavailable evidence
// rather than an empty healthy history.
func loadSnapshot(ctx context.Context, cfg storeConfig) (snap storeSnapshot, available bool, err error) {
	info, err := os.Lstat(cfg.dir)
	if err != nil {
		if isNotExist(err) {
			return storeSnapshot{}, false, nil
		}

		return storeSnapshot{}, false, fmt.Errorf("activity: inspect store directory: %w", err)
	}

	if err := dirSafe(info); err != nil {
		return storeSnapshot{}, false, err
	}
	// Read state before listing segments. A writer persists a segment before
	// writing its state, so a state observed first can never claim an event
	// that the later listing does not contain; the reverse order produced
	// false "evidence missing" verdicts on healthy concurrent appends. A
	// segment that retention removes while its bound is read restarts the
	// whole observation; see observeStore.
	snap, err = observeStore(ctx, cfg.dir, cfg, true)
	if err != nil {
		return storeSnapshot{}, false, err
	}

	return snap, true, nil
}

// snapshotFacts is the evidence verdict derived from one store snapshot.
type snapshotFacts struct {
	state       storeState
	stateExists bool
	segments    []segmentInfo
	storeID     string
	stateLost   bool
	ret         retainedBounds
	high        uint64
}

// deriveFacts turns a snapshot into the verdict every read phase uses.
func deriveFacts(snap storeSnapshot) (snapshotFacts, error) {
	stateCorrupt := errors.Is(snap.stateErr, errCorruptState)
	if snap.stateErr != nil && !stateCorrupt {
		return snapshotFacts{}, snap.stateErr
	}

	facts := snapshotFacts{
		state:       snap.state,
		stateExists: snap.stateExists,
		segments:    snap.segments,
		ret:         snap.ret,
		stateLost:   stateCorrupt || evidenceMissing(snap, snap.stateExists),
	}
	if !snap.stateExists && hasSegmentHistory(snap.segments) {
		facts.stateLost = true
	}

	if snap.stateExists {
		facts.storeID = snap.state.StoreID
	}

	facts.high = highwater(snap.state, snap.stateExists, snap.ret)

	return facts, nil
}

// evidenceMissing reports whether state counts events the retained segments
// cannot account for. A listed segment that vanished during the bound read
// means state and bounds come from different snapshots, so the stale bound is
// not proof that evidence is missing.
func evidenceMissing(snap storeSnapshot, stateOK bool) bool {
	return !snap.vanished && stateOK && snap.state.LastSequence > 0 &&
		(!snap.ret.hasLast || snap.ret.lastSeq < snap.state.LastSequence)
}

// baseCoverage is the coverage every page starts from.
func baseCoverage(facts snapshotFacts) Coverage {
	cov := Coverage{
		Available:       true,
		FirstRetainedAt: retainedFirstString(facts.ret),
		LastRetainedAt:  retainedLastString(facts.ret),
	}
	if facts.stateExists {
		cov.DroppedEvents = facts.state.DroppedEvents
	}

	return cov
}

// cursorPosition resolves the scan window a cursor requests. A cursor whose
// store instance or retained prefix cannot be verified ends the page instead of
// silently restarting.
func cursorPosition(nq normalizedQuery, facts snapshotFacts, cov Coverage) (after, snapshot uint64, expired *Coverage, err error) {
	after, snapshot = 0, facts.high
	if nq.cur == nil {
		return after, snapshot, nil, nil
	}

	after, snapshot = nq.cur.After, nq.cur.Snapshot

	if !facts.stateExists {
		// Without state the store instance and the loss counters cannot be
		// verified, and the cursor cannot be trusted to continue safely.
		gone := cov
		gone.CursorExpired = true
		gone.Partial = true
		gone.LossDetected = true

		return 0, 0, &gone, nil
	}

	if nq.cur.StoreID != facts.storeID {
		return 0, 0, nil, errCursorMismatch
	}

	if cursorExpired(nq.cur, facts.ret, facts.high, len(facts.segments)) {
		gone := cov
		gone.CursorExpired = true
		gone.Partial = true
		gone.LossDetected = facts.stateLost || cov.DroppedEvents > 0

		return 0, 0, &gone, nil
	}

	return after, snapshot, nil, nil
}

// pageScan accumulates the evidence one page returns while walking segments.
type pageScan struct {
	nq         normalizedQuery
	cfg        storeConfig
	budget     int64
	after      uint64
	snapshot   uint64
	segments   []segmentInfo
	matches    []Event
	walk       segmentWalk
	truncated  bool
	guaranteed bool
}

// run walks the listed segments under the page budget and collects matches.
func (ps *pageScan) run(ctx context.Context) error {
	for i, seg := range ps.segments {
		if err := ctx.Err(); err != nil {
			return err
		}

		stop, err := ps.scanSegment(ctx, i, seg)
		if err != nil {
			return err
		}

		if stop {
			break
		}
	}

	return nil
}

// scanSegment reads at most its budget share of one segment.
func (ps *pageScan) scanSegment(ctx context.Context, i int, seg segmentInfo) (bool, error) {
	if seg.size == 0 {
		return false, nil
	}

	if ps.guaranteed && ps.walk.bytes >= ps.budget {
		ps.truncated = true
		return true, nil
	}

	if ps.after > 0 && ps.skipBeforeCursor(ctx, seg) {
		return false, nil
	}

	limit, cutAtLimit, done := ps.segmentLimit(seg)
	if done {
		return true, nil
	}

	f, openErr := openSegmentRead(seg)
	if openErr != nil {
		return ps.failedOpen(openErr, cutAtLimit)
	}

	walk, walkErr := walkSegment(ctx, f, limit, cutAtLimit, i == len(ps.segments)-1, ps.walk.prevSeq, ps.collector())
	_ = f.Close()

	if walkErr != nil {
		return ps.failedWalk(ctx, cutAtLimit)
	}

	ps.mergeWalk(walk)

	return cutAtLimit || ps.walk.stoppedByVisit, nil
}

// skipBeforeCursor reports whether seg cannot contribute an event after the
// cursor. It skips the content without decoding it, but keeps the sequence
// baseline so later gap counts stay honest for the scanned range.
func (ps *pageScan) skipBeforeCursor(ctx context.Context, seg segmentInfo) bool {
	last, ok := segmentLastEvent(ctx, seg, ps.cfg.segmentBytes+maxEventBytes)
	if !ok || last.Sequence > ps.after {
		return false
	}

	if last.Sequence > ps.walk.prevSeq {
		ps.walk.prevSeq = last.Sequence
	}

	return true
}

// segmentLimit is how many bytes of seg this page may read. The first segment
// of a page is read whole (up to the absolute cap) so a page always makes
// progress instead of re-reading the same budget prefix behind the cursor
// forever; later segments share the remaining budget. done reports that no
// budget remains.
func (ps *pageScan) segmentLimit(seg segmentInfo) (limit int64, cutAtLimit, done bool) {
	limit = seg.size

	if !ps.guaranteed {
		ps.guaranteed = true
		if limit > maxPageBytes {
			ps.truncated = true
			return maxPageBytes, true, false
		}

		return limit, false, false
	}

	remaining := ps.budget - ps.walk.bytes
	if remaining <= 0 {
		ps.truncated = true
		return 0, false, true
	}

	if remaining < limit {
		ps.truncated = true
		return remaining, true, false
	}

	return limit, false, false
}

// failedOpen classifies an open failure for the page scan.
func (ps *pageScan) failedOpen(openErr error, cutAtLimit bool) (bool, error) {
	if errors.Is(openErr, errUnsafeSegment) {
		return false, openErr
	}

	if errors.Is(openErr, fs.ErrNotExist) {
		// Retention removed the segment after it was listed.
		ps.truncated = true
		return false, nil
	}

	ps.walk.malformedSegments++
	if cutAtLimit {
		ps.truncated = true
		return true, nil
	}

	return false, nil
}

// failedWalk classifies a walk failure for the page scan.
func (ps *pageScan) failedWalk(ctx context.Context, cutAtLimit bool) (bool, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}

	ps.walk.malformedSegments++
	if cutAtLimit {
		ps.truncated = true
		return true, nil
	}

	return false, nil
}

// collector records matching events and stops the walk once the page holds one
// more than the limit.
func (ps *pageScan) collector() func(Event) bool {
	return func(ev Event) bool {
		if ev.Sequence <= ps.after || ev.Sequence > ps.snapshot {
			return true
		}

		if !matchesVerifiedOperator(ps.nq.filter.VerifiedOperatorID, ev) || !matchesQuery(ps.nq, ev) {
			return true
		}

		ps.matches = append(ps.matches, ev)

		return len(ps.matches) <= ps.nq.limit
	}
}

// mergeWalk folds one walked segment into the page scan.
func (ps *pageScan) mergeWalk(walk segmentWalk) {
	walk.merge(&ps.walk)

	ps.walk.segments++
	if walk.inFlightTail {
		// A writer is still appending the newest record: this page is a prefix
		// of the retained evidence, not a complete scan of it.
		ps.truncated = true
	}
}

// exactRetainedBounds is the retained window proven by a complete scan.
func exactRetainedBounds(walk segmentWalk) retainedBounds {
	return retainedBounds{
		firstSeq: walk.first.Sequence,
		firstAt:  walk.earliest,
		hasFirst: true,
		lastSeq:  walk.last.Sequence,
		lastAt:   walk.latest,
		hasLast:  true,
	}
}

// coverageFromScan fills the counters derived from one page scan. matches is
// already trimmed to the page limit.
func coverageFromScan(cov *Coverage, nq normalizedQuery, ps *pageScan) {
	cov.EventsReturned = len(ps.matches)
	cov.EventsScanned = ps.walk.events
	cov.SegmentsScanned = ps.walk.segments
	cov.BytesScanned = ps.walk.bytes
	cov.MalformedSegments = ps.walk.malformedSegments
	cov.MalformedEvents = ps.walk.malformedEvents
	cov.SequenceGaps = ps.walk.gaps
	cov.Truncated = ps.truncated

	cov.LimitReached = len(ps.matches) == nq.limit && nq.limit > 0
	if len(ps.matches) > 0 {
		cov.FirstReturnedAt, cov.LastReturnedAt = returnedWindow(ps.matches)
	}
}

// returnedWindow is the earliest and latest occurrence actually returned.
func returnedWindow(matches []Event) (first, last string) {
	first, last = matches[0].OccurredAt, matches[0].OccurredAt
	for _, ev := range matches[1:] {
		first = earlierString(first, ev.OccurredAt)
		last = laterString(last, ev.OccurredAt)
	}

	return first, last
}

// continuation is the cursor a bounded page exposes. A page that could not
// advance past its cursor never repeats it: it reports a bounded, incomplete
// read with no continuation through stalled.
func continuation(nq normalizedQuery, ps *pageScan, after, snapshot uint64, storeID string, more, returnedAll bool) (next string, stalled bool) {
	if !more && !ps.truncated {
		return "", false
	}

	afterSeq := uint64(0)
	if len(ps.matches) > 0 {
		afterSeq = ps.matches[len(ps.matches)-1].Sequence
	}

	if ps.truncated && returnedAll && ps.walk.lastScannedSeq > afterSeq {
		afterSeq = ps.walk.lastScannedSeq
	}

	if afterSeq <= after {
		return "", true
	}

	return cursorFor(nq, afterSeq, snapshot, storeID), false
}

// finishVerdict marks evidence loss and partial reads from the final scan state.
func finishVerdict(cov *Coverage, nq normalizedQuery, snap storeSnapshot, ret retainedBounds, stalled bool, segmentCount int, stateLost bool) {
	cov.LossDetected = stateLost || cov.DroppedEvents > 0 ||
		cov.MalformedSegments > 0 || cov.MalformedEvents > 0 || cov.SequenceGaps > 0
	cov.Partial = cov.LossDetected || snap.vanished || stalled ||
		windowBeforeRetained(nq, ret, segmentCount)
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
func readRetainedBounds(ctx context.Context, segments []segmentInfo, cfg storeConfig) (retainedBounds, bool) {
	var (
		ret      retainedBounds
		vanished bool
	)

	ret.firstSeq, ret.firstAt, ret.hasFirst, vanished = firstRetainedEdge(segments, cfg)

	var lastVanished bool

	ret.lastSeq, ret.lastAt, ret.hasLast, lastVanished = lastRetainedEdge(ctx, segments, cfg)
	vanished = vanished || lastVanished

	if ret.hasFirst && ret.hasLast && ret.lastAt.Before(ret.firstAt) {
		ret.lastAt = ret.firstAt
	}

	return ret, vanished
}

// firstRetainedEdge is the oldest decodable event within the retained window.
func firstRetainedEdge(segments []segmentInfo, cfg storeConfig) (seq uint64, at time.Time, ok, vanished bool) {
	edge := cfg.segmentBytes + maxEventBytes
	budget := scanBudget(cfg)

	for _, seg := range segments {
		if seg.size <= 0 {
			continue
		}

		if budget <= 0 {
			break
		}

		segmentBudget := min(budget, edge)

		ev, found, err := segmentFirstEventErr(seg, segmentBudget)
		if isNotExist(err) {
			vanished = true
		}

		if found {
			return ev.Sequence, eventTime(ev), true, vanished
		}

		budget -= seg.size
	}

	return 0, time.Time{}, false, vanished
}

// lastRetainedEdge is the newest decodable event within the retained window.
func lastRetainedEdge(ctx context.Context, segments []segmentInfo, cfg storeConfig) (seq uint64, at time.Time, ok, vanished bool) {
	edge := cfg.segmentBytes + maxEventBytes
	budget := scanBudget(cfg)

	for _, seg := range slices.Backward(segments) {
		if seg.size <= 0 {
			continue
		}

		if budget <= 0 {
			break
		}

		segmentBudget := min(budget, edge)

		ev, found, err := segmentLastEventErr(ctx, seg, segmentBudget)
		if isNotExist(err) {
			vanished = true
		}

		if found {
			return ev.Sequence, eventTime(ev), true, vanished
		}

		budget -= seg.size
	}

	return 0, time.Time{}, false, vanished
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
func matchesVerifiedOperator(id string, ev Event) bool {
	return id == "" || ev.ActorVerified && ev.OperatorID == id
}

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
