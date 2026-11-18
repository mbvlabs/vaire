//go:build linux

package node

import "syscall"

// statDevice widens the kernel's device number to the width persisted in
// ownership and reservation identities. Linux already stores it that wide.
func statDevice(st *syscall.Stat_t) uint64 { return st.Dev }
