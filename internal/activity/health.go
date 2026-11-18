package activity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Health inspects the store without creating, repairing or truncating
// anything. Unsafe paths, unsafe modes, a missing state file with retained
// history and malformed data are reported through typed fields, never through
// free-form error text, and no secret is loaded. A missing directory is a
// normal result with Exists false.
//
// A read-only lock-file check reports an unusable private lock: the store is
// then not OwnerOnly, not Writable and LossDetected, because Append would
// refuse it. A missing lock is valid before the first append and is never
// created by Health.
func (s *Store) Health(ctx context.Context) (Health, error) {
	ctx = requestContext(ctx)

	cfg, err := s.config()
	if err != nil {
		return Health{}, err
	}

	h := newHealthReport(cfg)
	if err := ctx.Err(); err != nil {
		return h, err
	}

	info, err := os.Lstat(cfg.dir)
	if err != nil {
		if !isNotExist(err) {
			// The path exists but cannot be inspected. Report incomplete
			// evidence rather than an error string.
			h.LossDetected = true
		}

		return h, nil
	}

	dirOwnerOnly, ok := inspectHealthDir(cfg, info, &h)
	if !ok {
		return h, nil
	}
	// Read state before listing for the same reason as Query: a segment is
	// durable before its state, so state must never appear ahead of a later
	// listing. A segment removed while its retained bound is read restarts the
	// observation; the resulting snapshot is either consistent or marked
	// vanished rather than being mistaken for loss.
	snap, err := observeStore(ctx, cfg.dir, cfg, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return h, ctxErr
		}

		h.Readable = false
		h.LossDetected = true

		return h, nil
	}

	reportSnapshot(snap, &h)

	if h.Readable {
		if err := scanHealth(ctx, cfg, snap.segments, &h); err != nil {
			return h, err
		}
	}

	finishHealth(snap, dirOwnerOnly, &h)

	return h, nil
}

// newHealthReport seeds the reported store configuration.
func newHealthReport(cfg storeConfig) Health {
	return Health{
		Dir:          cfg.dir,
		MaxBytes:     cfg.maxBytes,
		SegmentBytes: cfg.segmentBytes,
		RetentionS:   int64(cfg.retention / time.Second),
	}
}

// inspectHealthDir reports the directory facts. It returns false when the path
// is not a directory at all, which ends the report as loss.
func inspectHealthDir(cfg storeConfig, info os.FileInfo, h *Health) (ownerOnly, ok bool) {
	h.Exists = true

	h.DirMode = octalMode(info.Mode().Perm())
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		h.LossDetected = true
		return false, false
	}

	h.IsDir = true
	h.Readable = isReadable(cfg.dir)
	h.Writable = isWritable(cfg.dir)

	return info.Mode().Perm() == dirMode && ownerMatches(info), true
}

// reportSnapshot folds the snapshot facts that the optional scan does not
// derive into the report.
func reportSnapshot(snap storeSnapshot, h *Health) {
	segments := snap.segments

	h.Segments = len(segments)
	for _, seg := range segments {
		if seg.regular {
			h.UsedBytes += seg.size
		}

		if !seg.safe() {
			h.NonOwnerOnlySegments++
		}
	}

	if len(segments) > 0 && segments[len(segments)-1].regular {
		h.SegmentMode = octalMode(segments[len(segments)-1].mode)
	}

	if snap.stateErr == nil && snap.stateExists {
		h.DroppedEvents = snap.state.DroppedEvents
		h.LastWriteAt = snap.state.LastWriteAt
	}

	h.FirstRetainedAt = retainedFirstString(snap.ret)
	h.LastRetainedAt = retainedLastString(snap.ret)
}

// finishHealth derives the ownership and loss verdict after the optional scan.
func finishHealth(snap storeSnapshot, dirOwnerOnly bool, h *Health) {
	lockUnsafe := lockFileUnsafe(h.Dir)
	stateOK := snap.stateErr == nil && snap.stateExists
	// A listed segment that vanished during the bound read makes state and
	// bounds different snapshots; state may still name an event in the fresh
	// listing. observeStore has retried, so a remaining inconsistency leaves
	// the comparison unproven: it is incomplete, not evidence loss.
	stateLoss := snap.stateErr != nil ||
		(!snap.stateExists && hasSegmentHistory(snap.segments)) ||
		evidenceMissing(snap, stateOK)
	h.OverBudget = h.UsedBytes > h.MaxBytes

	h.OwnerOnly = dirOwnerOnly && h.NonOwnerOnlySegments == 0 && !lockUnsafe
	if lockUnsafe {
		// Append would refuse the lock; a healthy writable claim would lie.
		h.Writable = false
	}

	h.LossDetected = h.MalformedSegments > 0 || h.MalformedEvents > 0 ||
		h.DroppedEvents > 0 || h.SequenceGaps > 0 ||
		h.NonOwnerOnlySegments > 0 || !dirOwnerOnly || stateLoss ||
		snap.moreEntries || lockUnsafe
}

// lockFileUnsafe reports whether the lock exists with an unusable shape. A
// missing lock is valid. It never creates or changes the lock.
func lockFileUnsafe(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, lockName))
	if err != nil {
		return !isNotExist(err)
	}

	return !info.Mode().IsRegular() || info.Mode().Perm() != fileMode || !ownerMatches(info)
}

// scanHealth walks every owner-only segment within the store's read budget and
// fills the malformed, gap and retained-window counters.
func scanHealth(ctx context.Context, cfg storeConfig, segments []segmentInfo, h *Health) error {
	scan := &healthScan{segments: segments, budget: scanBudget(cfg)}
	if err := scan.run(ctx); err != nil {
		return err
	}

	h.MalformedSegments += scan.walk.malformedSegments
	h.MalformedEvents = scan.walk.malformedEvents

	h.SequenceGaps = scan.walk.gaps
	if scan.walk.hasFirst {
		h.FirstRetainedAt = formatTime(scan.walk.earliest)
		if !scan.truncated {
			h.LastRetainedAt = formatTime(scan.walk.latest)
		}
	}

	return nil
}

// healthScan is one bounded health walk over the store's segments.
type healthScan struct {
	segments  []segmentInfo
	budget    int64
	walk      segmentWalk
	truncated bool
}

// run walks the segments until the budget is spent or the scan is cut.
func (hs *healthScan) run(ctx context.Context) error {
	for i, seg := range hs.segments {
		if err := ctx.Err(); err != nil {
			return err
		}

		if hs.walk.bytes >= hs.budget {
			hs.truncated = true
			break
		}

		stop, err := hs.scanSegment(ctx, i, seg)
		if err != nil {
			return err
		}

		if stop {
			break
		}
	}

	return nil
}

// scanSegment reads at most the remaining budget of one segment.
func (hs *healthScan) scanSegment(ctx context.Context, i int, seg segmentInfo) (bool, error) {
	if !seg.safe() {
		// Reported as NonOwnerOnlySegments; reading it would trust an unsafe
		// file.
		return false, nil
	}

	if seg.size == 0 {
		return false, nil
	}

	limit := seg.size
	cutAtLimit := false

	if remaining := hs.budget - hs.walk.bytes; remaining < limit {
		limit = remaining
		cutAtLimit = true
		hs.truncated = true
	}

	f, openErr := openSegmentRead(seg)
	if openErr != nil {
		if !errors.Is(openErr, errUnsafeSegment) && !errors.Is(openErr, fs.ErrNotExist) {
			hs.walk.malformedSegments++
		}

		return false, nil
	}

	walk, walkErr := walkSegment(ctx, f, limit, cutAtLimit, i == len(hs.segments)-1, hs.walk.prevSeq, func(Event) bool { return true })
	_ = f.Close()

	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}

		hs.walk.malformedSegments++

		return false, nil
	}

	walk.merge(&hs.walk)

	hs.walk.segments++
	if walk.inFlightTail {
		// A live append means this scan is a prefix of the retained evidence,
		// so it cannot pin the newest retained time.
		hs.truncated = true
	}

	return cutAtLimit, nil
}

// octalMode renders a permission mask as the store's Health contract expects.
func octalMode(mode os.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}
