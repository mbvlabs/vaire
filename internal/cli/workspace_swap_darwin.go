package cli

import "golang.org/x/sys/unix"

func swapWorkspaceDirs(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_SWAP)
}
