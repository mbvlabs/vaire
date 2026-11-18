//go:build linux

package backup

import "syscall"

// statDevice, statInode and statLinks widen the kernel's stat fields to the
// widths the reservation identity persists. Linux arm64 uses a narrower link
// count than amd64.
func statDevice(st *syscall.Stat_t) uint64 { return st.Dev }

func statInode(st *syscall.Stat_t) uint64 { return st.Ino }

func statLinks(st *syscall.Stat_t) uint64 {
	return uint64(st.Nlink) //nolint:unconvert // Linux arm64's Nlink is uint32.
}
