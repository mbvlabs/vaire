package masterbackup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// testRoot lets tests point MarkDirty and IsFenced at a scratch directory.
var testRoot = ""

// MarkDirty touches the dirty flag: a bundled file changed, and the path unit
// vaire-master-backup.path starts the job that bundles it. It never fails the
// caller: a missing directory (a host before master-backup-v1, or the
// operator's machine) is ignored, and the daily safety net catches anything
// lost.
func MarkDirty(reason string) {
	path := testRoot + DirtyPath
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return
	}
	now := time.Now()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: fixed node path.
	if err != nil {
		return
	}
	_, _ = f.WriteString(reason + "\n")
	_ = f.Close()
	_ = os.Chtimes(path, now, now)
}

// Dirty reports whether the flag is set, and when it was last touched.
func Dirty(root string) (bool, time.Time) {
	info, err := os.Stat(root + DirtyPath)
	if err != nil {
		return false, time.Time{}
	}
	return true, info.ModTime()
}

// ClearDirty removes the flag before a snapshot, so a change during the
// build sets it again.
func ClearDirty(root string) error {
	err := os.Remove(root + DirtyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SetTestRoot points MarkDirty and IsFenced under root, for tests in other
// packages, and returns the function that restores them.
func SetTestRoot(root string) func() {
	old := testRoot
	testRoot = root
	return func() { testRoot = old }
}

// Settled reports whether the flag has been quiet for at least quiet: the
// job's debounce.
func Settled(touched, now time.Time, quiet time.Duration) bool {
	return !now.Before(touched.Add(quiet))
}
