package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// WriteFileAtomic writes data to path through a temp file in the same
// directory: write, fsync, chmod, rename, then fsync the directory. The file
// keeps the owner of the process that writes it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return WriteFileAtomicOwned(path, data, perm, "", "")
}

// WriteFileAtomicOwned is WriteFileAtomic with an owner and group. The temp
// file is chowned before the rename, so a service that has to read the file
// never sees it with the wrong owner. An empty owner or group leaves that
// field as the writing process has it.
func WriteFileAtomicOwned(path string, data []byte, perm os.FileMode, owner, group string) (err error) {
	uid, gid, err := ResolveIDs(owner, group)
	if err != nil {
		return err
	}
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
	if uid != -1 || gid != -1 {
		if err = f.Chown(uid, gid); err != nil {
			_ = f.Close()
			return err
		}
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

// ResolveIDs turns an owner and group name into the numeric ids os.Chown
// wants, leaving a name that is empty at -1 so it is not changed.
func ResolveIDs(owner, group string) (uid, gid int, err error) {
	uid, gid = -1, -1
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return 0, 0, fmt.Errorf("look up user %q: %w", owner, err)
		}
		if uid, err = strconv.Atoi(u.Uid); err != nil {
			return 0, 0, fmt.Errorf("user %q has a non-numeric uid %q", owner, u.Uid)
		}
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return 0, 0, fmt.Errorf("look up group %q: %w", group, err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return 0, 0, fmt.Errorf("group %q has a non-numeric gid %q", group, g.Gid)
		}
	}
	return uid, gid, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: directory of a fixed node path.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
