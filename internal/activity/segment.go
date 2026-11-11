package activity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Segment file layout. Names carry a zero-padded creation index, so lexical
// order is append order and a rotation never reuses an existing name.
const (
	segmentPrefix   = "seg-"
	segmentSuffix   = ".ndjson"
	segmentIndexLen = 16
	segmentNameLen  = len(segmentPrefix) + segmentIndexLen + len(segmentSuffix)

	// hardMaxSegments bounds directory enumeration, append rotation and read
	// scanning. The byte budget normally keeps the count far lower.
	hardMaxSegments = 64

	maxDirEntries = 2*hardMaxSegments + 16
	dirReadBatch  = 32

	// hardScanCap bounds one query or health scan even if an operator
	// configures a larger byte budget. A single page may read one whole
	// segment to guarantee progress, but never more than this.
	hardScanCap int64 = 64 << 20
)

// maxPageBytes is the absolute read resource cap for one page or health scan.
// It is a variable only so tests can exercise the bound cheaply; production
// code never changes it.
var maxPageBytes int64 = hardScanCap

// dirEntryHook, when set by a test, runs after a directory entry is read and
// before its metadata is inspected. It exists to reproduce a retention delete
// racing the enumeration deterministically.
var dirEntryHook func(name string)

// segmentInfo is one segment file as observed without following symlinks.
type segmentInfo struct {
	index   uint64
	name    string
	path    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	ownerOK bool
	regular bool
}

// safe reports whether the segment is a regular, owner-only file. Anything else
// is refused rather than repaired.
func (s segmentInfo) safe() bool {
	return s.regular && s.ownerOK && s.mode == fileMode
}

// segmentFileName renders the name for creation index.
func segmentFileName(index uint64) string {
	return segmentPrefix + fmt.Sprintf("%016x", index) + segmentSuffix
}

// parseSegmentName accepts exactly one well-formed segment name.
func parseSegmentName(name string) (uint64, bool) {
	if len(name) != segmentNameLen || !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
		return 0, false
	}
	index, err := strconv.ParseUint(name[len(segmentPrefix):len(segmentPrefix)+segmentIndexLen], 16, 64)
	if err != nil || index == 0 {
		return 0, false
	}
	return index, true
}

// listSegments enumerates the store directory without following symlinks. It
// reads at most maxDirEntries entries; more reports that the bound was reached,
// and raced reports that an entry vanished between ReadDir and its metadata
// read (a concurrent retention delete). A vanished entry is skipped rather than
// turning a healthy read into an error. Unknown files are ignored so a future
// store version or an operator's note cannot break reads.
func listSegments(dir string) ([]segmentInfo, bool, bool, error) {
	f, err := os.Open(dir) //nolint:gosec // G304: the store directory.
	if err != nil {
		return nil, false, false, fmt.Errorf("activity: read store directory: %w", err)
	}
	defer func() { _ = f.Close() }()
	var (
		segments []segmentInfo
		entries  int
		more     bool
		raced    bool
	)
	for {
		batch, readErr := f.ReadDir(dirReadBatch)
		for _, entry := range batch {
			entries++
			if entries > maxDirEntries {
				more = true
				break
			}
			name := entry.Name()
			if name == stateName || name == lockName || strings.HasPrefix(name, ".") {
				continue
			}
			index, ok := parseSegmentName(name)
			if !ok {
				continue
			}
			if dirEntryHook != nil {
				dirEntryHook(name)
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				if errors.Is(infoErr, fs.ErrNotExist) {
					// Retention removed the segment after ReadDir. The
					// evidence set changed under us; skip it and let callers
					// qualify the scan.
					raced = true
					continue
				}
				return nil, false, raced, fmt.Errorf("activity: read segment entry: %w", infoErr)
			}
			uidOK := ownerMatches(info)
			segments = append(segments, segmentInfo{
				index:   index,
				name:    name,
				path:    filepath.Join(dir, name),
				size:    info.Size(),
				mode:    info.Mode().Perm(),
				modTime: info.ModTime(),
				ownerOK: uidOK,
				regular: info.Mode().IsRegular(),
			})
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, false, raced, fmt.Errorf("activity: read store directory: %w", readErr)
		}
		if more || readErr != nil {
			break
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].index < segments[j].index })
	for i := 1; i < len(segments); i++ {
		if segments[i].index == segments[i-1].index {
			return nil, false, raced, errUnsafeSegment
		}
	}
	return segments, more, raced, nil
}

// openSegmentRead opens a segment read-only without following symlinks and
// re-checks the safety properties observed during enumeration.
func openSegmentRead(seg segmentInfo) (*os.File, error) {
	f, err := os.OpenFile(seg.path, os.O_RDONLY|noFollowFlag, 0) //nolint:gosec // G304: a listed store segment.
	if err != nil {
		return nil, err
	}
	if err := verifyOpenSegment(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// verifyOpenSegment rejects a swapped file between listing and open.
func verifyOpenSegment(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != fileMode || !ownerMatches(info) {
		return errUnsafeSegment
	}
	return nil
}

// segmentFirstEvent returns the first decodable event of a segment, reading at
// most budget bytes. Errors and malformed data yield ok false.
func segmentFirstEvent(seg segmentInfo, budget int64) (Event, bool) {
	ev, ok, _ := segmentFirstEventErr(seg, budget)
	return ev, ok
}

// segmentFirstEventErr is segmentFirstEvent that also reports an open failure,
// so a caller comparing retained bounds with state can tell a segment removed
// by retention from one with no decodable event.
func segmentFirstEventErr(seg segmentInfo, budget int64) (Event, bool, error) {
	if seg.size <= 0 {
		return Event{}, false, nil
	}
	if budget <= 0 || budget > seg.size {
		budget = seg.size
	}
	f, err := openSegmentRead(seg)
	if err != nil {
		return Event{}, false, err
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(io.LimitReader(f, budget), 4096)
	for {
		line, terminated, oversize, readErr := readEventLine(reader, maxEventBytes)
		if line == nil && errors.Is(readErr, io.EOF) {
			return Event{}, false, nil
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Event{}, false, nil
		}
		if terminated && !oversize && len(line) > 0 {
			if ev, decErr := decodeStoredEvent(line); decErr == nil {
				return ev, true, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return Event{}, false, nil
		}
	}
}

// segmentLastEvent returns the last decodable event of a segment. It reads only
// the tail that can contain the last line, because a stored line is bounded by
// maxEventBytes.
func segmentLastEvent(seg segmentInfo, budget int64) (Event, bool) {
	ev, ok, _ := segmentLastEventErr(seg, budget)
	return ev, ok
}

// segmentLastEventErr is segmentLastEvent that also reports an open failure,
// so a caller comparing retained bounds with state can tell a segment removed
// by retention from one with no decodable event.
func segmentLastEventErr(seg segmentInfo, budget int64) (Event, bool, error) {
	if seg.size <= 0 {
		return Event{}, false, nil
	}
	if budget <= 0 || budget > seg.size {
		budget = seg.size
	}
	chunk := int64(2 * maxEventBytes)
	if chunk > budget {
		chunk = budget
	}
	start := seg.size - chunk
	f, err := openSegmentRead(seg)
	if err != nil {
		return Event{}, false, err
	}
	defer func() { _ = f.Close() }()
	raw := make([]byte, chunk)
	if _, err := f.ReadAt(raw, start); err != nil && !errors.Is(err, io.EOF) {
		return Event{}, false, nil
	}
	// The tail may begin in the middle of a line; only complete trailing lines
	// are considered.
	if start > 0 {
		if idx := bytes.IndexByte(raw, '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			return Event{}, false, nil
		}
	}
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) > 0 {
		// The file does not end with a newline. The final fragment is a torn
		// write, not evidence; never let it become a stored event.
		lines = lines[:len(lines)-1]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if len(line) == 0 {
			continue
		}
		if len(line) > maxEventBytes {
			continue
		}
		if ev, decErr := decodeStoredEvent(line); decErr == nil {
			return ev, true, nil
		}
	}
	if start > 0 {
		// A run of malformed tail data larger than the tail window can hide
		// the last valid event. Fall back to one bounded full scan rather
		// than reporting no tail at all.
		ev, ok := segmentLastEventScan(f, budget, budget < seg.size)
		return ev, ok, nil
	}
	return Event{}, false, nil
}

// segmentLastEventScan walks a whole bounded segment to find its last valid
// event. It is only used when the tail window cannot be decoded.
func segmentLastEventScan(f *os.File, budget int64, cutAtLimit bool) (Event, bool) {
	walk, err := walkSegment(context.Background(), f, budget, cutAtLimit, false, 0, func(Event) bool { return true })
	if err != nil || !walk.hasLast {
		return Event{}, false
	}
	return walk.last, true
}

// segmentEndsWithNewline reports whether the last byte is a newline, so a
// torn write is never merged with a later event. An empty segment is clean.
func segmentEndsWithNewline(seg segmentInfo) (bool, error) {
	if seg.size == 0 {
		return true, nil
	}
	f, err := openSegmentRead(seg)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var last [1]byte
	if _, err := f.ReadAt(last[:], seg.size-1); err != nil {
		return false, err
	}
	return last[0] == '\n', nil
}

// readEventLine reads one newline-terminated line while keeping memory bounded
// by max. Oversized lines are consumed and reported instead of aborting the
// scan, so one hostile line cannot hide later evidence. terminated is false
// when the input ended before a newline: the caller decides whether that is a
// scan-budget cutoff or a genuine torn write. The returned line is nil only at a
// clean EOF.
func readEventLine(reader *bufio.Reader, max int) (line []byte, terminated bool, oversize bool, err error) {
	for {
		chunk, readErr := reader.ReadSlice('\n')
		if !oversize {
			if len(line)+len(chunk) > max {
				oversize = true
				if len(line) < max {
					line = append(line, chunk[:max-len(line)]...)
				}
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case readErr == nil:
			return line, true, oversize, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF):
			if len(line) == 0 && !oversize {
				return nil, false, false, io.EOF
			}
			return line, false, oversize, nil
		default:
			return nil, false, false, readErr
		}
	}
}

// decodeStoredEvent strictly decodes one stored event: unknown fields at any
// level are rejected, as is a value outside the typed allowlist.
func decodeStoredEvent(line []byte) (Event, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	if len(line) == 0 {
		return Event{}, errMalformedEvent
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var ev Event
	if err := dec.Decode(&ev); err != nil {
		return Event{}, errMalformedEvent
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return Event{}, errMalformedEvent
	}
	if err := validateStoredEvent(ev); err != nil {
		return Event{}, errMalformedEvent
	}
	return ev, nil
}

// encodeEvent renders one canonical NDJSON line for ev.
func encodeEvent(ev Event) ([]byte, error) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("activity: encode event: %w", err)
	}
	raw = append(raw, '\n')
	if len(raw) > maxEventBytes {
		return nil, errInvalidEvent
	}
	return raw, nil
}

// segmentWalk is the bounded result of walking one segment.
type segmentWalk struct {
	bytes             int64
	segments          int
	malformedSegments int
	malformedEvents   int
	events            int
	gaps              uint64
	prevSeq           uint64
	first             Event
	hasFirst          bool
	last              Event
	hasLast           bool
	earliest          time.Time
	latest            time.Time
	lastScannedSeq    uint64
	stoppedByVisit    bool
	// inFlightTail reports that the walk stopped at a line another process was
	// still writing: the file grew past this reader's snapshot, or a writer
	// holds the store's append lock. The missing newline is then not
	// corruption, but the walk is also not a complete scan of the segment.
	inFlightTail bool
}

// walkSegment reads at most maxBytes from f, decoding every event. visit
// returns false to stop the walk early. cutAtLimit says the reader stopped at a
// caller budget rather than at the file's true end, so an unterminated final
// line is truncation, not corruption. newestSegment says f is the last segment
// in the store, the only one an append can extend, so an unterminated final
// line may be a live write. Gaps are counted against prevSeq, which the caller
// carries across segments.
func walkSegment(ctx context.Context, f *os.File, maxBytes int64, cutAtLimit, newestSegment bool, prevSeq uint64, visit func(Event) bool) (walk segmentWalk, err error) {
	walk.prevSeq = prevSeq
	if maxBytes <= 0 {
		return walk, nil
	}
	counter := &byteCounter{r: io.LimitReader(f, maxBytes)}
	defer func() { walk.bytes = counter.n }()
	reader := bufio.NewReaderSize(counter, 4096)
	for lines := 0; ; lines++ {
		if lines%512 == 0 {
			if err := ctx.Err(); err != nil {
				return walk, err
			}
		}
		line, terminated, oversize, readErr := readEventLine(reader, maxEventBytes)
		if line == nil && errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return walk, fmt.Errorf("activity: read segment: %w", readErr)
		}
		if !terminated && cutAtLimit {
			// A budget boundary in the middle of a line is a clean scan cut.
			// The caller reports Truncated; nothing is corruption.
			break
		}
		switch {
		case !terminated:
			// A real torn tail: the file ended without the newline that every
			// stored record carries. A reader may also have caught a live
			// append of the newest segment, and only that one can still grow.
			if newestSegment && tailInFlight(f, counter.n) {
				walk.inFlightTail = true
				return walk, nil
			}
			walk.malformedEvents++
		case oversize:
			walk.malformedEvents++
		case len(line) > 0:
			ev, decErr := decodeStoredEvent(line)
			if decErr != nil {
				walk.malformedEvents++
			} else {
				walk.events++
				if walk.prevSeq != 0 && walk.prevSeq != ^uint64(0) && ev.Sequence > walk.prevSeq+1 {
					walk.gaps += ev.Sequence - walk.prevSeq - 1
				}
				if ev.Sequence > walk.prevSeq {
					walk.prevSeq = ev.Sequence
				}
				if ev.Sequence > walk.lastScannedSeq {
					walk.lastScannedSeq = ev.Sequence
				}
				if t := eventTime(ev); !t.IsZero() {
					if !walk.hasFirst || t.Before(walk.earliest) {
						walk.earliest = t
					}
					if !walk.hasLast || t.After(walk.latest) {
						walk.latest = t
					}
				}
				if !walk.hasFirst {
					walk.first, walk.hasFirst = ev, true
				}
				walk.last, walk.hasLast = ev, true
				if !visit(ev) {
					walk.stoppedByVisit = true
					return walk, nil
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return walk, nil
}

// afterTailStatHook, when set by a test, runs between the first size check and
// the append-lock probe of tailInFlight. It reproduces a writer that finishes
// and unlocks in exactly that window.
var afterTailStatHook func()

// tailInFlight reports whether an unterminated final line is still being
// written: the file grew past the snapshot this walk was given, or a writer
// holds the store's append lock. A paused writer keeps the lock while its line
// is incomplete, and a missing or unusable lock file is not treated as held,
// so a torn tail with no writer stays visible as corruption.
func tailInFlight(f *os.File, consumed int64) bool {
	if segmentGrew(f, consumed) {
		return true
	}
	if afterTailStatHook != nil {
		afterTailStatHook()
	}
	if appendLockHeld(filepath.Dir(f.Name())) {
		return true
	}
	// A writer that finished the line and released the lock between the size
	// check and the lock probe leaves a segment that grew but is no longer
	// locked; re-check the size so that transition is not called corruption.
	// An unchanged size with a free lock is still a torn tail.
	return segmentGrew(f, consumed)
}

// segmentGrew reports whether f holds more bytes than this reader consumed.
func segmentGrew(f *os.File, consumed int64) bool {
	info, err := f.Stat()
	return err == nil && info.Size() > consumed
}

// byteCounter counts bytes actually consumed, including bytes of an oversized
// line that were skipped.
type byteCounter struct {
	r io.Reader
	n int64
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// merge folds one walked segment into a running scan.
func (w *segmentWalk) merge(into *segmentWalk) {
	into.bytes += w.bytes
	into.segments += w.segments
	into.malformedSegments += w.malformedSegments
	into.malformedEvents += w.malformedEvents
	into.events += w.events
	into.gaps += w.gaps
	into.prevSeq = w.prevSeq
	if !into.hasFirst && w.hasFirst {
		into.first, into.hasFirst = w.first, true
	}
	if w.hasLast {
		into.last, into.hasLast = w.last, true
	}
	if w.hasFirst {
		if into.earliest.IsZero() || w.earliest.Before(into.earliest) {
			into.earliest = w.earliest
		}
		if into.latest.IsZero() || w.latest.After(into.latest) {
			into.latest = w.latest
		}
	}
	if w.lastScannedSeq > into.lastScannedSeq {
		into.lastScannedSeq = w.lastScannedSeq
	}
	if w.stoppedByVisit {
		into.stoppedByVisit = true
	}
	if w.inFlightTail {
		into.inFlightTail = true
	}
}

// eventTime parses a stored timestamp; stored events always pass validation,
// so a failure is treated as a zero time.
func eventTime(ev Event) time.Time {
	t, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// laterString returns the later of two stored timestamps.
func laterString(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	at, aErr := time.Parse(time.RFC3339Nano, a)
	bt, bErr := time.Parse(time.RFC3339Nano, b)
	if aErr != nil {
		return b
	}
	if bErr != nil {
		return a
	}
	if bt.After(at) {
		return b
	}
	return a
}

// earlierString returns the earlier of two stored timestamps.
func earlierString(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	at, aErr := time.Parse(time.RFC3339Nano, a)
	bt, bErr := time.Parse(time.RFC3339Nano, b)
	if aErr != nil {
		return b
	}
	if bErr != nil {
		return a
	}
	if bt.Before(at) {
		return b
	}
	return a
}

// scanBudget bounds one read. The configured whole-store total is the natural
// bound, but the absolute page cap always applies; it is never raised to a
// configured segment size.
func scanBudget(cfg storeConfig) int64 {
	budget := cfg.maxBytes + cfg.segmentBytes
	if budget > maxPageBytes {
		budget = maxPageBytes
	}
	minRead := int64(maxEventBytes)
	if maxPageBytes < minRead {
		minRead = maxPageBytes
	}
	if budget < minRead {
		budget = minRead
	}
	return budget
}

// segmentSafeCheck is the shared refusal for a store whose files are not
// owner-only.
func segmentSafeCheck(segs []segmentInfo) error {
	for _, seg := range segs {
		if !seg.safe() {
			return errUnsafeSegment
		}
	}
	return nil
}

// isNotExist reports whether err is a missing path.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
