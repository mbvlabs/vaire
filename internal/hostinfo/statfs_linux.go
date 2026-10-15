//go:build linux

package hostinfo

import "syscall"

// statfsGiB returns the filesystem size in GiB for path, or ok=false.
func statfsGiB(path string) (int, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize) //nolint:gosec // G115: Bsize is a positive filesystem block size.
	if total == 0 {
		return 0, false
	}
	return int(total / (1 << 30)), true //nolint:gosec // G115: a filesystem smaller than 8 EiB fits in int.
}

// FreeBytes returns the bytes available to unprivileged users on the
// filesystem holding path, or ok=false.
func FreeBytes(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true //nolint:gosec // G115: Bsize is a positive filesystem block size.
}

// DiskUsage returns the size of the filesystem holding path and the bytes
// available to unprivileged users, or ok=false.
func DiskUsage(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize) //nolint:gosec // G115: Bsize is a positive filesystem block size.
	return st.Blocks * bs, st.Bavail * bs, st.Blocks > 0
}
