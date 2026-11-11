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
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := s.config()
	if err != nil {
		return Health{}, err
	}
	h := Health{
		Dir:          cfg.dir,
		MaxBytes:     cfg.maxBytes,
		SegmentBytes: cfg.segmentBytes,
		RetentionS:   int64(cfg.retention / time.Second),
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}
	info, err := os.Lstat(cfg.dir) //nolint:gosec // G304: the store directory.
	if err != nil {
		if !isNotExist(err) {
			// The path exists but cannot be inspected. Report incomplete
			// evidence rather than an error string.
			h.LossDetected = true
		}
		return h, nil
	}
	h.Exists = true
	h.DirMode = octalMode(info.Mode().Perm())
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		h.LossDetected = true
		return h, nil
	}
	h.IsDir = true
	h.Readable = isReadable(cfg.dir)
	h.Writable = isWritable(cfg.dir)
	dirOwnerOnly := info.Mode().Perm() == dirMode && ownerMatches(info)
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
	state, stateExists, stateErr := snap.state, snap.stateExists, snap.stateErr
	segments, moreEntries := snap.segments, snap.moreEntries
	h.Segments = len(segments)
	for _, seg := range segments {
		if seg.regular {
			h.UsedBytes += seg.size
		}
		if !seg.regular || !seg.ownerOK || seg.mode != fileMode {
			h.NonOwnerOnlySegments++
		}
	}
	if len(segments) > 0 {
		newest := segments[len(segments)-1]
		if newest.regular {
			h.SegmentMode = octalMode(newest.mode)
		}
	}
	lockUnsafe := lockFileUnsafe(cfg.dir)
	stateOK := stateErr == nil && stateExists
	ret := snap.ret
	// A listed segment that vanished during the bound read makes state and
	// bounds different snapshots; state may still name an event in the fresh
	// listing. observeStore has retried, so a remaining inconsistency leaves
	// the comparison unproven: it is incomplete, not evidence loss.
	evidenceMissing := !snap.vanished && stateOK && state.LastSequence > 0 &&
		(!ret.hasLast || ret.lastSeq < state.LastSequence)
	stateLoss := stateErr != nil || (!stateExists && hasSegmentHistory(segments)) || evidenceMissing
	if stateOK {
		h.DroppedEvents = state.DroppedEvents
		h.LastWriteAt = state.LastWriteAt
	}
	h.FirstRetainedAt = retainedFirstString(ret)
	h.LastRetainedAt = retainedLastString(ret)
	if h.Readable {
		if err := scanHealth(ctx, cfg, segments, &h); err != nil {
			return h, err
		}
	}
	h.OverBudget = h.UsedBytes > h.MaxBytes
	h.OwnerOnly = dirOwnerOnly && h.NonOwnerOnlySegments == 0 && !lockUnsafe
	if lockUnsafe {
		// Append would refuse the lock; a healthy writable claim would lie.
		h.Writable = false
	}
	h.LossDetected = h.MalformedSegments > 0 || h.MalformedEvents > 0 ||
		h.DroppedEvents > 0 || h.SequenceGaps > 0 ||
		h.NonOwnerOnlySegments > 0 || !dirOwnerOnly || stateLoss ||
		moreEntries || lockUnsafe
	return h, nil
}

// lockFileUnsafe reports whether the lock exists with an unusable shape. A
// missing lock is valid. It never creates or changes the lock.
func lockFileUnsafe(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, lockName)) //nolint:gosec // G304: the store's own lock path.
	if err != nil {
		return !isNotExist(err)
	}
	return !info.Mode().IsRegular() || info.Mode().Perm() != fileMode || !ownerMatches(info)
}

// scanHealth walks every owner-only segment within the store's read budget and
// fills the malformed, gap and retained-window counters.
func scanHealth(ctx context.Context, cfg storeConfig, segments []segmentInfo, h *Health) error {
	budget := scanBudget(cfg)
	var (
		scanned   segmentWalk
		truncated bool
	)
	for i, seg := range segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scanned.bytes >= budget {
			truncated = true
			break
		}
		if !seg.regular || !seg.ownerOK || seg.mode != fileMode {
			// Reported as NonOwnerOnlySegments; reading it would trust an
			// unsafe file.
			continue
		}
		if seg.size == 0 {
			continue
		}
		remaining := budget - scanned.bytes
		limit := seg.size
		cutAtLimit := false
		if remaining < limit {
			limit = remaining
			cutAtLimit = true
			truncated = true
		}
		f, err := openSegmentRead(seg)
		if err != nil {
			if errors.Is(err, errUnsafeSegment) || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			scanned.malformedSegments++
			continue
		}
		walk, err := walkSegment(ctx, f, limit, cutAtLimit, i == len(segments)-1, scanned.prevSeq, func(Event) bool { return true })
		_ = f.Close()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			scanned.malformedSegments++
			continue
		}
		walk.merge(&scanned)
		scanned.segments++
		if walk.inFlightTail {
			// A live append means this scan is a prefix of the retained
			// evidence, so it cannot pin the newest retained time.
			truncated = true
		}
		if cutAtLimit {
			break
		}
	}
	h.MalformedSegments += scanned.malformedSegments
	h.MalformedEvents = scanned.malformedEvents
	h.SequenceGaps = scanned.gaps
	if scanned.hasFirst {
		h.FirstRetainedAt = formatTime(scanned.earliest)
		if !truncated {
			h.LastRetainedAt = formatTime(scanned.latest)
		}
	}
	return nil
}

// octalMode renders a permission mask as the store's Health contract expects.
func octalMode(mode os.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}
