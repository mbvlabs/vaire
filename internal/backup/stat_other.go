//go:build !linux

package backup

import "syscall"

// statDevice, statInode and statLinks widen the kernel's stat fields to the
// widths the reservation identity persists; other Unix systems use narrower
// fields.
func statDevice(st *syscall.Stat_t) uint64 { return uint64(st.Dev) }

func statInode(st *syscall.Stat_t) uint64 { return uint64(st.Ino) }

func statLinks(st *syscall.Stat_t) uint64 { return uint64(st.Nlink) }
