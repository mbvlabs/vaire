package activity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Store filenames and state bounds.
const (
	stateName     = "state.json"
	lockName      = "lock"
	stateVersion  = 1
	maxStateBytes = 4096
)

// storeState is the store's derived bookkeeping and private store identity.
// Events remain the only evidence. The dropped counter and the store identity
// cannot be rebuilt from segments, which is why a corrupt or missing state is
// refused by Append instead of being silently replaced. The document is a
// private v1 format decoded strictly: unknown fields are refused, not ignored.
type storeState struct {
	Version       int    `json:"version"`
	StoreID       string `json:"store_id"`
	NextSequence  uint64 `json:"next_sequence"`
	FirstSequence uint64 `json:"first_sequence,omitempty"`
	LastSequence  uint64 `json:"last_sequence,omitempty"`
	FirstEventAt  string `json:"first_event_at,omitempty"`
	LastEventAt   string `json:"last_event_at,omitempty"`
	LastWriteAt   string `json:"last_write_at,omitempty"`
	DroppedEvents uint64 `json:"dropped_events,omitempty"`
	ActiveSegment uint64 `json:"active_segment,omitempty"`
	ActiveBytes   int64  `json:"active_bytes,omitempty"`
}

// valid reports whether st is internally consistent. It never guesses. The
// store identity is required: it binds cursors to this store instance.
func (st storeState) valid() bool {
	if st.Version != stateVersion || st.NextSequence == 0 || !validStoreID(st.StoreID) {
		return false
	}
	if st.LastSequence >= st.NextSequence {
		return false
	}
	if st.FirstSequence > st.LastSequence {
		return false
	}
	if st.FirstSequence == 0 {
		if st.LastSequence != 0 || st.FirstEventAt != "" || st.LastEventAt != "" {
			return false
		}
	} else if st.FirstEventAt == "" || st.LastEventAt == "" {
		return false
	}
	for _, value := range []string{st.FirstEventAt, st.LastEventAt, st.LastWriteAt} {
		if value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return false
		}
	}
	if st.ActiveSegment == 0 && st.ActiveBytes != 0 {
		return false
	}
	if st.ActiveBytes < 0 {
		return false
	}
	return true
}

// readState loads state.json with bounded memory and strict decoding. A missing
// file is not an error; an unsafe or corrupt file is.
func readState(dir string) (storeState, bool, error) {
	path := filepath.Join(dir, stateName)
	info, err := os.Lstat(path)
	if err != nil {
		if isNotExist(err) {
			return storeState{}, false, nil
		}
		return storeState{}, false, fmt.Errorf("activity: inspect state file: %w", err)
	}
	if !info.Mode().IsRegular() || !ownerMatches(info) || info.Mode().Perm() != fileMode {
		return storeState{}, false, errUnsafeState
	}
	if info.Size() > maxStateBytes {
		return storeState{}, false, errCorruptState
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0) //nolint:gosec // G304: the store's own state file.
	if err != nil {
		if isNotExist(err) {
			return storeState{}, false, nil
		}
		if errors.Is(err, fs.ErrPermission) {
			return storeState{}, false, errUnsafeState
		}
		return storeState{}, false, fmt.Errorf("activity: open state file: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return storeState{}, false, fmt.Errorf("activity: read state file: %w", err)
	}
	if len(raw) > maxStateBytes {
		return storeState{}, false, errCorruptState
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var st storeState
	if err := dec.Decode(&st); err != nil {
		return storeState{}, false, errCorruptState
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return storeState{}, false, errCorruptState
	}
	if !st.valid() {
		return storeState{}, false, errCorruptState
	}
	return st, true, nil
}

// writeState writes state.json atomically and durably: temp file, fsync,
// rename, directory fsync.
func writeState(dir string, st storeState) (err error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("activity: encode state: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".state.json.")
	if err != nil {
		return fmt.Errorf("activity: create state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("activity: chmod state file: %w", err)
	}
	if _, err = tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("activity: write state file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("activity: sync state file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("activity: close state file: %w", err)
	}
	if err = os.Rename(tmpName, filepath.Join(dir, stateName)); err != nil {
		return fmt.Errorf("activity: replace state file: %w", err)
	}
	if err = syncDir(dir); err != nil {
		return fmt.Errorf("activity: sync store directory: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a created, renamed or removed entry is durable.
func syncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // G304: the store directory.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
