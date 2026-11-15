//go:build linux

package backup

import "syscall"

// statDevice, statInode and statLinks widen the kernel's stat fields to the
// widths the reservation identity persists; on Linux they already have those
// widths.
func statDevice(st *syscall.Stat_t) uint64 { return st.Dev }

func statInode(st *syscall.Stat_t) uint64 { return st.Ino }

func statLinks(st *syscall.Stat_t) uint64 { return st.Nlink }
