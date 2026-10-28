//go:build unix

package activity

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// noFollowFlag makes file opens refuse a final-component symlink.
const noFollowFlag = syscall.O_NOFOLLOW

// ownerMatches reports whether info belongs to the effective user. Ownership is
// checked, never corrected.
func ownerMatches(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid() //nolint:gosec // G115: a uid fits in int.
}

// isReadable checks access without opening or modifying the path.
func isReadable(path string) bool {
	return unix.Access(path, unix.R_OK) == nil
}

// isWritable checks access without creating a probe file.
func isWritable(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}
