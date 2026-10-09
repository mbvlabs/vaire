package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/mbvlabs/eru/internal/secrets"
)

// LogDir holds one log per RPC or job.
const LogDir = "/var/log/eru"

var logNamePart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RunLog is the per-request log under LogDir. Every write is redacted. A nil
// RunLog discards writes, so dry runs can pass nil.
type RunLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// OpenRunLog creates <LogDir>/<UTC ts>-<method>-<request_id>.log, mode 0640,
// in a 0750 directory.
func OpenRunLog(method, requestID string) (*RunLog, error) {
	if !logNamePart.MatchString(method) || !logNamePart.MatchString(requestID) {
		return nil, fmt.Errorf("run log name has unsafe characters")
	}
	if err := os.MkdirAll(LogDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chmod(LogDir, 0o750); err != nil { //nolint:gosec // G302: §6.2 sets /var/log/eru to 0750.
		return nil, err
	}
	name := fmt.Sprintf("%s-%s-%s.log", time.Now().UTC().Format("20060102T150405Z"), method, requestID)
	path := filepath.Join(LogDir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o640) //nolint:gosec // G302,G304: §6.2 run log mode under a fixed directory.
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o640); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &RunLog{f: f, path: path}, nil
}

// Path returns the log file path.
func (l *RunLog) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Line writes one redacted, timestamped line.
func (l *RunLog) Line(kind, text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = fmt.Fprintf(l.f, "%s %s %s\n", ts, kind, secrets.Redact(text))
}

// JSON writes v as one redacted JSON line.
func (l *RunLog) JSON(kind string, v any) {
	if l == nil {
		return
	}
	raw, err := json.Marshal(secrets.RedactValue(v))
	if err != nil {
		l.Line(kind, err.Error())
		return
	}
	l.Line(kind, string(raw))
}

// Close flushes and closes the log.
func (l *RunLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
