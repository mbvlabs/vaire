//go:build linux

package node

import "golang.org/x/sys/unix"

// renameNoReplaceAt atomically renames from into to relative to the pinned
// directories, refusing to replace an existing destination with EEXIST.
func renameNoReplaceAt(fromDir int, from string, toDir int, to string) error {
	return unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
}
