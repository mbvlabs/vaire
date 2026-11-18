//go:build !linux

package node

import "syscall"

// statDevice widens the kernel's device number to the width persisted in
// ownership and reservation identities; Darwin stores dev_t as int32.
func statDevice(st *syscall.Stat_t) uint64 { return uint64(st.Dev) }
