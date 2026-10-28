//go:build !unix

package activity

import (
	"io/fs"
	"os"
)

// noFollowFlag is unavailable; the common directory checks still reject a
// symlinked store because they use Lstat.
const noFollowFlag = 0

// ownerMatches cannot compare uids on this platform; the mode checks still
// apply.
func ownerMatches(fs.FileInfo) bool { return true }

// isReadable falls back to mode bits.
func isReadable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o444 != 0
}

// isWritable falls back to mode bits.
func isWritable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o200 != 0
}
