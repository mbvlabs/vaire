package backup

import (
	"bytes"
	"errors"
	"io"
	"os"
)

const recoveryDiagnosticPollLimit = 256 * 1024
const recoveryDiagnosticContextLimit = 16 * 1024

// recoveryLogCursor only reads evidence appended after its creation. A changed
// identity or truncation is unknown evidence, not an empty successful read.
type recoveryLogCursor struct {
	path        string
	initial     os.FileInfo
	offset      int64
	context     []byte
	backlog     bool
	partial     bool
	skipOldLine bool
	observedNew bool
}

func newRecoveryLogCursor(path string) (*recoveryLogCursor, error) {
	c := &recoveryLogCursor{path: path}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("recovery log is not a regular file")
	}
	c.initial, c.offset = info, info.Size()
	if c.offset > 0 {
		f, err := os.Open(path) //nolint:gosec // G304: fixed cluster log path.
		if err != nil {
			return nil, err
		}
		var last [1]byte
		n, readErr := f.ReadAt(last[:], c.offset-1)
		opened, statErr := f.Stat()
		_ = f.Close()
		if readErr != nil || statErr != nil || n != 1 || !os.SameFile(info, opened) {
			return nil, errors.New("cannot establish recovery log cursor")
		}
		c.skipOldLine = last[0] != '\n'
	}
	return c, nil
}

func (c *recoveryLogCursor) read() (string, error) {
	return c.readLimit(recoveryDiagnosticPollLimit)
}

func (c *recoveryLogCursor) readLimit(limit int) (string, error) {
	f, err := os.Open(c.path) //nolint:gosec // G304: caller supplies the fixed cluster log path.
	if err != nil {
		if c.initial == nil && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || (c.initial != nil && !os.SameFile(c.initial, info)) || info.Size() < c.offset {
		return "", errors.New("recovery log changed identity or was truncated")
	}
	if c.initial == nil {
		c.initial = info
	}
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return "", err
	}
	buf, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return "", err
	}
	c.offset += int64(len(buf))
	if len(buf) > 0 {
		c.observedNew = true
	}
	c.backlog = info.Size() > c.offset
	if c.skipOldLine {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
			c.skipOldLine = false
		} else {
			c.partial = true
			return "", nil
		}
	}
	all := append(c.context, buf...)
	c.partial = len(all) > 0 && all[len(all)-1] != '\n'
	result := string(all)
	if len(all) > recoveryDiagnosticContextLimit {
		c.context = append(c.context[:0], all[len(all)-recoveryDiagnosticContextLimit:]...)
	} else {
		c.context = append(c.context[:0], all...)
	}
	return result, nil
}
