package engine

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a temp file in the same
// directory: write, fsync, chmod, rename, then fsync the directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(suffix))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: tmp is beside a fixed node path.
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: directory of a fixed node path.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
